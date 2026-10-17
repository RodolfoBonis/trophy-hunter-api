package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai/claude"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

const validTrophyGuide = `{"where":"Capítulo final","summary":"Vença o jogo.","steps":["Jogue a campanha","Derrote o chefe final"],"prerequisites":[],
"missable":false,"point_of_no_return":"","difficulty":3,"estimated_time":"8 h","tips":["Use o modo fácil"],
"confidence":0.8,"uncertainty_notes":"","sources":["https://example.com/guia"]}`

const validRoadmap = `{"summary":"Duas jogadas.","difficulty":4,"estimated_total_time":"20 h","playthroughs":2,
"stages":[{"title":"Campanha","description":"Jogue normalmente.","trophies":["Segredo"],"missables":[]}],
"missables_overview":[],"tips":[],"confidence":0.7,"uncertainty_notes":"Jogo de nicho.","sources":[]}`

// fakeRunner imita a CLI: transmite o JSON/texto em pedaços e devolve o resultado.
type fakeRunner struct {
	mu       sync.Mutex
	reqs     []claude.Request
	override func(claude.Request) (claude.Result, bool)
}

func (f *fakeRunner) Run(_ context.Context, req claude.Request, on func(claude.Event)) (claude.Result, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	override := f.override
	f.mu.Unlock()
	if override != nil {
		if res, ok := override(req); ok {
			return res, nil
		}
	}
	switch {
	case strings.Contains(req.JSONSchema, `"stages"`):
		return chunked(on, claude.EventJSON, validRoadmap, claude.Result{Structured: []byte(validRoadmap), SessionID: "11111111-1111-1111-1111-111111111111"}), nil
	case req.JSONSchema != "":
		on(claude.Event{Type: claude.EventTool, Text: "Pesquisando: portal 2 venceu"})
		return chunked(on, claude.EventJSON, validTrophyGuide, claude.Result{Structured: []byte(validTrophyGuide)}), nil
	default:
		answer := "Você ainda pode pegar o Segredo."
		return chunked(on, claude.EventText, answer, claude.Result{Text: answer, SessionID: "22222222-2222-2222-2222-222222222222"}), nil
	}
}

func (f *fakeRunner) last() claude.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func chunked(on func(claude.Event), typ claude.EventType, s string, res claude.Result) claude.Result {
	for i := 0; i < len(s); i += 7 {
		on(claude.Event{Type: typ, Text: s[i:min(i+7, len(s))]})
	}
	return res
}

// drainAI imita o ai-worker: Claim (só ai_*) → Handle → Complete/Fail. Devolve os erros.
func (e *testEnv) drainAI(t *testing.T) []error {
	t.Helper()
	ctx := context.Background()
	var errs []error
	for i := 0; i < 20; i++ {
		job, err := e.queue.Claim(ctx, time.Minute, ai.KindGuide, ai.KindChat)
		require.NoError(t, err)
		if job == nil {
			return errs
		}
		if herr := e.ai.Handle(ctx, *job); herr != nil {
			errs = append(errs, herr)
			require.NoError(t, e.queue.Fail(ctx, job.ID, job.LeaseToken, herr))
			continue
		}
		require.NoError(t, e.queue.Complete(ctx, job.ID, job.LeaseToken))
	}
	return errs
}

type sseEvent struct{ ID, Type, Data string }

// readSSE consome a stream até o evento final (ou timeout).
func (e *testEnv) readSSE(t *testing.T, id, lastID string) []sseEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+"/api/v1/ai/streams/"+id, nil)
	require.NoError(t, err)
	req.Header.Set("X-Debug-User", "me")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	var out []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.ID = line[4:]
		case strings.HasPrefix(line, "event: "):
			cur.Type = line[7:]
		case strings.HasPrefix(line, "data: "):
			var d struct{ Data string }
			require.NoError(t, json.Unmarshal([]byte(line[6:]), &d))
			cur.Data = d.Data
		case line == "":
			if cur.Type != "" && cur.Type != "ping" {
				out = append(out, cur)
				if cur.Type == "done" || cur.Type == "error" {
					return out
				}
			}
			cur = sseEvent{}
		}
	}
	return out
}

type guideResp struct {
	ID            string          `json:"id"`
	Scope         string          `json:"scope"`
	Status        string          `json:"status"`
	Content       json.RawMessage `json:"content"`
	EditedContent json.RawMessage `json:"edited_content"`
	Error         string          `json:"error"`
}

func setupAI(t *testing.T, r *fakeRunner) (*testEnv, string, string) {
	t.Helper()
	e := build(t, func(string) syncpkg.SteamAPI { return fakeSteam{} }, nil, r)
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"K","steam_id_or_vanity":"76561197960000000"}`, http.StatusCreated)
	e.drain(t)
	page := call[struct {
		Items []struct{ ID string }
	}](t, e, http.MethodGet, "/api/v1/games", "", http.StatusOK)
	require.Len(t, page.Items, 1)
	gameID := page.Items[0].ID
	detail := call[struct {
		Groups []struct {
			Trophies []struct{ ID, Name string }
		}
	}](t, e, http.MethodGet, "/api/v1/games/"+gameID, "", http.StatusOK)
	var trophyID string
	for _, tr := range detail.Groups[0].Trophies {
		if tr.Name == "Venceu" {
			trophyID = tr.ID
		}
	}
	require.NotEmpty(t, trophyID)
	return e, gameID, trophyID
}

func TestTrophyGuideStreamsAndPersists(t *testing.T) {
	r := &fakeRunner{}
	e, _, trophyID := setupAI(t, r)
	base := "/api/v1/trophies/" + trophyID + "/guide"

	none := call[struct{ Guide *guideResp }](t, e, http.MethodGet, base, "", http.StatusOK)
	require.Nil(t, none.Guide)

	g := call[guideResp](t, e, http.MethodPost, base, "", http.StatusAccepted)
	require.Equal(t, "pending", g.Status)

	// O browser conecta antes do worker: recebe a geração ao vivo.
	got := make(chan []sseEvent, 1)
	go func() { got <- e.readSSE(t, g.ID, "") }()
	time.Sleep(300 * time.Millisecond)
	require.Empty(t, e.drainAI(t))
	events := <-got

	var partial strings.Builder
	var tool string
	for _, ev := range events {
		switch ev.Type {
		case "json":
			partial.WriteString(ev.Data)
		case "tool":
			tool = ev.Data
		}
	}
	require.Equal(t, "done", events[len(events)-1].Type)
	require.Equal(t, "Pesquisando: portal 2 venceu", tool)
	require.JSONEq(t, validTrophyGuide, partial.String(), "os pedaços remontam o guia")

	req := r.last()
	require.Contains(t, req.Prompt, "Portal 2")
	require.Contains(t, req.Prompt, "Venceu")
	require.Equal(t, ai.AllowedTools, req.AllowedTools)
	require.Equal(t, "opus", req.Model, "guias usam o modelo de guia")
	require.NotEmpty(t, req.JSONSchema)

	ready := call[struct{ Guide *guideResp }](t, e, http.MethodGet, base, "", http.StatusOK)
	require.Equal(t, "ready", ready.Guide.Status)
	require.JSONEq(t, validTrophyGuide, string(ready.Guide.Content))
	require.Equal(t, "null", string(ready.Guide.EditedContent))

	// Stream expirada: a reconexão recebe só o evento final.
	again := e.readSSE(t, g.ID, events[len(events)-1].ID)
	require.Empty(t, again[:len(again)-1])

	// Edição do usuário: validada pelo mesmo schema; null volta ao gerado.
	edited := strings.Replace(validTrophyGuide, "Vença o jogo.", "Vença no difícil.", 1)
	ed := call[guideResp](t, e, http.MethodPatch, "/api/v1/ai/guides/"+g.ID, `{"edited_content":`+edited+`}`, http.StatusOK)
	require.Contains(t, string(ed.EditedContent), "Vença no difícil.")
	call[struct{}](t, e, http.MethodPatch, "/api/v1/ai/guides/"+g.ID, `{"edited_content":{"summary":"x"}}`, http.StatusUnprocessableEntity)
	reset := call[guideResp](t, e, http.MethodPatch, "/api/v1/ai/guides/"+g.ID, `{"edited_content":null}`, http.StatusOK)
	require.Equal(t, "null", string(reset.EditedContent))

	// Regenerar preserva a edição e volta para pending.
	call[guideResp](t, e, http.MethodPatch, "/api/v1/ai/guides/"+g.ID, `{"edited_content":`+edited+`}`, http.StatusOK)
	regen := call[guideResp](t, e, http.MethodPost, base, "", http.StatusAccepted)
	require.Equal(t, g.ID, regen.ID)
	require.Equal(t, "pending", regen.Status)
	require.Contains(t, string(regen.EditedContent), "Vença no difícil.")
}

func TestGuideFailuresAndRateLimit(t *testing.T) {
	r := &fakeRunner{}
	e, gameID, trophyID := setupAI(t, r)

	// Limite de uso: erro transitório (reagenda), guia continua pendente, a UI recebe um aviso.
	r.override = func(claude.Request) (claude.Result, bool) { return claude.Result{RateLimited: true}, true }
	g := call[guideResp](t, e, http.MethodPost, "/api/v1/trophies/"+trophyID+"/guide", "", http.StatusAccepted)
	errs := e.drainAI(t)
	require.Len(t, errs, 1)
	cur := call[struct{ Guide *guideResp }](t, e, http.MethodGet, "/api/v1/trophies/"+trophyID+"/guide", "", http.StatusOK)
	require.Equal(t, "running", cur.Guide.Status)

	// Saída fora do schema: erro definitivo com mensagem amigável.
	r.override = func(claude.Request) (claude.Result, bool) {
		return claude.Result{Structured: []byte(`{"summary":"só isso"}`)}, true
	}
	rm := call[guideResp](t, e, http.MethodPost, "/api/v1/games/"+gameID+"/roadmap", "", http.StatusAccepted)
	require.Equal(t, "roadmap", rm.Scope)
	got := make(chan []sseEvent, 1)
	go func() { got <- e.readSSE(t, rm.ID, "") }()
	time.Sleep(300 * time.Millisecond)
	// Força o job de roadmap (o de troféu está em backoff).
	e.drainAI(t)
	events := <-got
	require.Equal(t, "error", events[len(events)-1].Type)
	require.Contains(t, events[len(events)-1].Data, "incompleto")
	failed := call[struct{ Guide *guideResp }](t, e, http.MethodGet, "/api/v1/games/"+gameID+"/roadmap", "", http.StatusOK)
	require.Equal(t, "error", failed.Guide.Status)
	_ = g

	// Sucesso no roadmap após regenerar.
	r.override = nil
	call[guideResp](t, e, http.MethodPost, "/api/v1/games/"+gameID+"/roadmap", "", http.StatusAccepted)
	e.drainAI(t)
	ok := call[struct{ Guide *guideResp }](t, e, http.MethodGet, "/api/v1/games/"+gameID+"/roadmap", "", http.StatusOK)
	require.Equal(t, "ready", ok.Guide.Status)
	require.Contains(t, r.last().Prompt, "Segredo", "o roadmap leva o que falta")
}

type chatMsg struct {
	ID, Role, Content, Status, Error string
}

func TestChatResumesAndRebuildsContext(t *testing.T) {
	r := &fakeRunner{}
	e, gameID, _ := setupAI(t, r)
	base := "/api/v1/games/" + gameID + "/chat"

	empty := call[struct{ Messages []chatMsg }](t, e, http.MethodGet, base, "", http.StatusOK)
	require.Empty(t, empty.Messages)
	call[struct{}](t, e, http.MethodPost, base, `{"message":"   "}`, http.StatusUnprocessableEntity)

	sent := call[struct{ User, Assistant chatMsg }](t, e, http.MethodPost, base, `{"message":"Perdi algo?"}`, http.StatusAccepted)
	require.Equal(t, "pending", sent.Assistant.Status)
	// Uma pergunta por vez.
	call[struct{}](t, e, http.MethodPost, base, `{"message":"E agora?"}`, http.StatusConflict)

	got := make(chan []sseEvent, 1)
	go func() { got <- e.readSSE(t, sent.Assistant.ID, "") }()
	time.Sleep(300 * time.Millisecond)
	require.Empty(t, e.drainAI(t))
	var text strings.Builder
	for _, ev := range <-got {
		if ev.Type == "text" {
			text.WriteString(ev.Data)
		}
	}
	require.Equal(t, "Você ainda pode pegar o Segredo.", text.String())
	first := r.last()
	require.Equal(t, "sonnet", first.Model, "chat usa o modelo rápido")
	require.Empty(t, first.ResumeID)
	require.Contains(t, first.Prompt, "Minha pergunta: Perdi algo?")
	require.Contains(t, first.Prompt, "Segredo", "contexto com o que falta")

	// 2ª pergunta: retoma a sessão; a sessão sumiu (pod recriado) → refaz com o histórico.
	r.override = func(req claude.Request) (claude.Result, bool) {
		if req.ResumeID != "" {
			return claude.Result{NoConversation: true}, true
		}
		return claude.Result{}, false
	}
	call[struct{ User, Assistant chatMsg }](t, e, http.MethodPost, base, `{"message":"E o DLC?"}`, http.StatusAccepted)
	require.Empty(t, e.drainAI(t))
	r.mu.Lock()
	resumed, rebuilt := r.reqs[len(r.reqs)-2], r.reqs[len(r.reqs)-1]
	r.mu.Unlock()
	require.Equal(t, "22222222-2222-2222-2222-222222222222", resumed.ResumeID)
	require.Contains(t, rebuilt.Prompt, "Conversa até agora:")
	require.Contains(t, rebuilt.Prompt, "Eu: Perdi algo?")
	require.Contains(t, rebuilt.Prompt, "Você: Você ainda pode pegar o Segredo.")
	require.Equal(t, first.Workdir, rebuilt.Workdir, "mesmo cwd por conversa (o --resume depende disso)")

	msgs := call[struct{ Messages []chatMsg }](t, e, http.MethodGet, base, "", http.StatusOK)
	require.Len(t, msgs.Messages, 4)
	require.Equal(t, "done", msgs.Messages[3].Status)

	// Nova conversa.
	res := e.do(t, http.MethodDelete, base, "", true)
	_ = res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	cleared := call[struct{ Messages []chatMsg }](t, e, http.MethodGet, base, "", http.StatusOK)
	require.Empty(t, cleared.Messages)
}

func TestAIDisabledWithoutRedis(t *testing.T) {
	e := setup(t)
	res := e.do(t, http.MethodPost, "/api/v1/games/00000000-0000-0000-0000-000000000001/chat", `{"message":"oi"}`, true)
	_ = res.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, res.StatusCode)
	res = e.do(t, http.MethodGet, "/api/v1/ai/streams/00000000-0000-0000-0000-000000000001", "", true)
	_ = res.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, res.StatusCode)
}
