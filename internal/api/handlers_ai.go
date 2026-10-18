package api

import (
	"context"

	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai/stream"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
)

// guideDTO ↔ components.schemas.AIGuide. content = gerado pelo modelo (guia de troféu é
// compartilhado); edited_content = correção PRIVADA de quem está vendo (a UI mostra a editada).
type guideDTO struct {
	ID            string          `json:"id"`
	Scope         string          `json:"scope"`
	Status        string          `json:"status"`
	Content       json.RawMessage `json:"content"`
	EditedContent json.RawMessage `json:"edited_content"`
	Model         string          `json:"model"`
	Error         string          `json:"error"`
	GeneratedAt   *time.Time      `json:"generated_at"`
	UpdatedAt     *time.Time      `json:"updated_at"`
}

type guideEnvelope struct {
	Guide *guideDTO `json:"guide"`
}

func toGuideDTO(g dbgen.AiGuide, edited []byte) guideDTO {
	return guideDTO{
		ID: uuidStr(g.ID), Scope: g.Scope, Status: g.Status,
		Content: rawOrNull(g.Content), EditedContent: rawOrNull(edited),
		Model: g.Model, Error: g.Error, GeneratedAt: tptr(g.GeneratedAt), UpdatedAt: tptr(g.UpdatedAt),
	}
}

// guideFor monta o DTO com a edição privada do usuário (se houver).
func (h *Handlers) guideFor(ctx context.Context, u dbgen.User, g dbgen.AiGuide) (guideDTO, error) {
	edited, err := h.q.GetGuideEdit(ctx, dbgen.GetGuideEditParams{UserID: u.ID, GuideID: g.ID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return guideDTO{}, err
	}
	return toGuideDTO(g, edited), nil
}

// canSeeGuide: guia de troféu é de todos; roadmap só do dono dele.
func canSeeGuide(u dbgen.User, g dbgen.AiGuide) bool {
	return g.Scope == ai.ScopeTrophy || g.UserID == u.ID
}

// writeAIRequestErr traduz os erros de pedido de IA (teto diário → 429 com código estável).
func writeAIRequestErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ai.ErrDailyLimit) {
		httperr.Write(w, r, httperr.New(httperr.KindRateLimited, "Limite diário de IA",
			"você atingiu o limite de pedidos de IA de hoje; tente amanhã").WithCode("ai_daily_limit"))
		return
	}
	writeInternal(w, r, err)
}

func rawOrNull(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}

func (h *Handlers) aiEnabled(w http.ResponseWriter, r *http.Request) bool {
	if h.ai == nil {
		httperr.Write(w, r, httperr.New(httperr.KindUnavailable, "IA indisponível", "a IA não está configurada"))
		return false
	}
	return true
}

func (h *Handlers) loadTrophyParam(w http.ResponseWriter, r *http.Request) (dbgen.GetTrophyWithGameRow, bool) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return dbgen.GetTrophyWithGameRow{}, false
	}
	t, err := h.q.GetTrophyWithGame(r.Context(), id)
	if err == nil && t.GameUserID != currentUser(r).ID {
		err = pgx.ErrNoRows // de outra pessoa: 404, sem revelar que existe
	}
	if err != nil {
		writeNotFoundOr(w, r, err, "troféu não encontrado")
		return t, false
	}
	return t, true
}

// writeGuide responde {guide} (null quando ainda não existe), com a edição privada do usuário.
func (h *Handlers) writeGuide(w http.ResponseWriter, r *http.Request, g dbgen.AiGuide, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, guideEnvelope{})
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	d, err := h.guideFor(r.Context(), currentUser(r), g)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, guideEnvelope{Guide: &d})
}

// getTrophyGuide — GET /trophies/{id}/guide (guia compartilhado do troféu).
func (h *Handlers) getTrophyGuide(w http.ResponseWriter, r *http.Request) {
	t, ok := h.loadTrophyParam(w, r)
	if !ok {
		return
	}
	g, err := h.q.GetTrophyGuide(r.Context(), dbgen.GetTrophyGuideParams{Platform: t.GamePlatform, GameExternalID: t.GameExternalID, TrophyExternalID: t.ExternalID})
	h.writeGuide(w, r, g, err)
}

// generateTrophyGuide — POST /trophies/{id}/guide: gera (ou regenera) o guia; resposta via stream.
func (h *Handlers) generateTrophyGuide(w http.ResponseWriter, r *http.Request) {
	if !h.aiEnabled(w, r) {
		return
	}
	t, ok := h.loadTrophyParam(w, r)
	if !ok {
		return
	}
	g, err := h.ai.RequestTrophyGuide(r.Context(), currentUser(r), t)
	if err != nil {
		writeAIRequestErr(w, r, err)
		return
	}
	d, err := h.guideFor(r.Context(), currentUser(r), g)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, d)
}

// getRoadmap — GET /games/{id}/roadmap (roadmap do usuário).
func (h *Handlers) getRoadmap(w http.ResponseWriter, r *http.Request) {
	game, ok := h.loadGameParam(w, r)
	if !ok {
		return
	}
	g, err := h.q.GetRoadmap(r.Context(), dbgen.GetRoadmapParams{UserID: currentUser(r).ID, Platform: game.Platform, GameExternalID: game.ExternalID})
	h.writeGuide(w, r, g, err)
}

// generateRoadmap — POST /games/{id}/roadmap.
func (h *Handlers) generateRoadmap(w http.ResponseWriter, r *http.Request) {
	if !h.aiEnabled(w, r) {
		return
	}
	game, ok := h.loadGameParam(w, r)
	if !ok {
		return
	}
	g, err := h.ai.RequestRoadmap(r.Context(), currentUser(r), game)
	if err != nil {
		writeAIRequestErr(w, r, err)
		return
	}
	d, err := h.guideFor(r.Context(), currentUser(r), g)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, d)
}

type editGuideRequest struct {
	// EditedContent: guia corrigido (mesmo schema) ou null para voltar ao gerado.
	EditedContent json.RawMessage `json:"edited_content"`
}

// editGuide — PATCH /ai/guides/{id}: grava a correção do usuário (validada pelo mesmo schema).
func (h *Handlers) editGuide(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return
	}
	u := currentUser(r)
	g, err := h.q.GetGuideByID(r.Context(), id)
	if err == nil && !canSeeGuide(u, g) {
		err = pgx.ErrNoRows
	}
	if err != nil {
		writeNotFoundOr(w, r, err, "guia não encontrado")
		return
	}
	var req editGuideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return
	}
	var edited []byte
	if len(req.EditedContent) > 0 && string(req.EditedContent) != "null" {
		edited, err = ai.ValidateGuide(g.Scope, req.EditedContent)
		if err != nil {
			httperr.Write(w, r, httperr.Validation(err.Error(), httperr.FieldError{Field: "edited_content", Code: "invalid"}))
			return
		}
	}
	// A correção é PRIVADA: só quem editou vê (o guia de troféu continua igual para os outros).
	if edited == nil {
		err = h.q.DeleteGuideEdit(r.Context(), dbgen.DeleteGuideEditParams{UserID: u.ID, GuideID: g.ID})
	} else {
		err = h.q.UpsertGuideEdit(r.Context(), dbgen.UpsertGuideEditParams{UserID: u.ID, GuideID: g.ID, Content: edited})
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toGuideDTO(g, edited))
}

// chatMessageDTO ↔ components.schemas.ChatMessage.
type chatMessageDTO struct {
	ID        string     `json:"id"`
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Status    string     `json:"status"`
	Error     string     `json:"error"`
	CreatedAt *time.Time `json:"created_at"`
}

func toChatDTO(m dbgen.ChatMessage) chatMessageDTO {
	return chatMessageDTO{ID: uuidStr(m.ID), Role: m.Role, Content: m.Content, Status: m.Status, Error: m.Error, CreatedAt: tptr(m.CreatedAt)}
}

// getChat — GET /games/{id}/chat: a conversa ativa do jogo (vazia se não houver).
func (h *Handlers) getChat(w http.ResponseWriter, r *http.Request) {
	g, ok := h.loadGameParam(w, r)
	if !ok {
		return
	}
	out := struct {
		Messages []chatMessageDTO `json:"messages"`
	}{Messages: []chatMessageDTO{}}
	th, err := h.q.GetActiveThread(r.Context(), dbgen.GetActiveThreadParams{UserID: currentUser(r).ID, Platform: g.Platform, GameExternalID: g.ExternalID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	msgs, err := h.q.ListThreadMessages(r.Context(), th.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, toChatDTO(m))
	}
	writeJSON(w, http.StatusOK, out)
}

// sendChat — POST /games/{id}/chat {message}: grava a pergunta e enfileira a resposta.
func (h *Handlers) sendChat(w http.ResponseWriter, r *http.Request) {
	if !h.aiEnabled(w, r) {
		return
	}
	g, ok := h.loadGameParam(w, r)
	if !ok {
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return
	}
	user, assistant, err := h.ai.SendChat(r.Context(), currentUser(r), g, req.Message)
	switch {
	case errors.Is(err, ai.ErrDailyLimit):
		writeAIRequestErr(w, r, err)
		return
	case errors.Is(err, ai.ErrEmptyChat):
		httperr.Write(w, r, httperr.Validation("mensagem vazia", httperr.FieldError{Field: "message", Code: "required"}))
		return
	case errors.Is(err, ai.ErrChatBusy):
		httperr.Write(w, r, httperr.New(httperr.KindConflict, "Conflict", err.Error()))
		return
	case err != nil:
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]chatMessageDTO{"user": toChatDTO(user), "assistant": toChatDTO(assistant)})
}

// resetChat — DELETE /games/{id}/chat: arquiva a conversa (a próxima pergunta começa do zero).
func (h *Handlers) resetChat(w http.ResponseWriter, r *http.Request) {
	g, ok := h.loadGameParam(w, r)
	if !ok {
		return
	}
	if err := h.q.ArchiveThread(r.Context(), dbgen.ArchiveThreadParams{UserID: currentUser(r).ID, Platform: g.Platform, GameExternalID: g.ExternalID}); err != nil {
		writeInternal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const (
	// sseBlock é o intervalo máximo sem bytes na conexão (heartbeat): abaixo dos timeouts de
	// proxy (Gravitee/Cloudflare).
	sseBlock = 5 * time.Second
	// sseMaxDuration encerra streams esquecidas; o browser reconecta com Last-Event-ID.
	sseMaxDuration = 15 * time.Minute
)

// aiStream — GET /ai/streams/{id} (SSE): a resposta da IA em tempo real. id = guia ou mensagem do
// assistente. Eventos: text/json/tool/status (parciais) e done/error (finais). Retoma do
// Last-Event-ID. Se a resposta já terminou e a stream expirou, manda só o evento final.
func (h *Handlers) aiStream(w http.ResponseWriter, r *http.Request) {
	if h.rdb == nil {
		httperr.Write(w, r, httperr.New(httperr.KindUnavailable, "IA indisponível", "stream indisponível"))
		return
	}
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return
	}
	key := uuidStr(id)
	final, ok := h.streamFinalState(w, r, id)
	if !ok {
		return
	}

	rc := http.NewResponseController(w)
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache, no-transform")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(evID, typ, data string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		payload, _ := json.Marshal(map[string]string{"data": data})
		var b strings.Builder
		if evID != "" {
			fmt.Fprintf(&b, "id: %s\n", evID)
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", typ, payload)
		if _, err := w.Write([]byte(b.String())); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	ctx := r.Context()
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = "0"
	}
	deadline := time.Now().Add(sseMaxDuration)
	first := true
	for ctx.Err() == nil && time.Now().Before(deadline) {
		evs, err := stream.Read(ctx, h.rdb, key, last, sseBlock)
		if err != nil {
			if ctx.Err() == nil {
				h.logger.WarnContext(ctx, "ai stream: leitura falhou", "err", err.Error())
			}
			return
		}
		if len(evs) == 0 {
			// Nada na stream: se a resposta já terminou (stream expirada), encerra com o final.
			if first && final != nil {
				send("", final.typ, final.data)
				return
			}
			if !send("", "ping", "") {
				return
			}
			first = false
			continue
		}
		first = false
		for _, e := range evs {
			last = e.ID
			if !send(e.ID, e.Type, e.Data) {
				return
			}
			if e.Final() {
				return
			}
		}
	}
}

type finalEvent struct{ typ, data string }

// streamFinalState confere que o id é um guia ou mensagem do chat e, se já terminou, devolve o
// evento final equivalente (para quando a stream do Redis já expirou).
func (h *Handlers) streamFinalState(w http.ResponseWriter, r *http.Request, id pgtype.UUID) (*finalEvent, bool) {
	ctx := r.Context()
	u := currentUser(r)
	notFound := func() (*finalEvent, bool) {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "resposta não encontrada"))
		return nil, false
	}
	if g, err := h.q.GetGuideByID(ctx, id); err == nil {
		if !canSeeGuide(u, g) {
			return notFound()
		}
		switch g.Status {
		case "ready":
			return &finalEvent{stream.TypeDone, ""}, true
		case "error":
			return &finalEvent{stream.TypeError, g.Error}, true
		}
		return nil, true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeInternal(w, r, err)
		return nil, false
	}
	m, err := h.q.GetChatMessageOwner(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeInternal(w, r, err)
		return nil, false
	}
	if err != nil || m.Role != "assistant" || m.UserID != u.ID {
		return notFound()
	}
	switch m.Status {
	case "done":
		return &finalEvent{stream.TypeDone, ""}, true
	case "error":
		return &finalEvent{stream.TypeError, m.Error}, true
	}
	return nil, true
}
