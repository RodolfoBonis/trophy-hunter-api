package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai/claude"
)

type huntResp struct {
	Roadmap           string   `json:"roadmap"`
	Syncing           bool     `json:"syncing"`
	MissablesOverview []string `json:"missables_overview"`
	Stages            []struct {
		Index     int      `json:"index"`
		Title     string   `json:"title"`
		TrophyIDs []string `json:"trophy_ids"`
		Unmatched []string `json:"unmatched"`
		Earned    int      `json:"earned"`
		Done      bool     `json:"done"`
		Complete  bool     `json:"complete"`
	} `json:"stages"`
}

const huntRoadmap = `{"summary":"Duas etapas.","difficulty":3,"estimated_total_time":"10 h","playthroughs":1,
"stages":[{"title":"Campanha","description":"Zere.","trophies":["venceu"],"missables":[]},
{"title":"Segredos","description":"Antes do fim.","trophies":["Segredo (perdível)","Troféu inventado"],"missables":["Segredo"]}],
"missables_overview":["Segredo antes do chefe final"],"tips":[],"confidence":0.8,"uncertainty_notes":"","sources":[]}`

const huntRoadmapOneStage = `{"summary":"Uma etapa.","difficulty":3,"estimated_total_time":"10 h","playthroughs":1,
"stages":[{"title":"Tudo","description":"Zere.","trophies":["Venceu","Segredo"],"missables":[]}],
"missables_overview":[],"tips":[],"confidence":0.8,"uncertainty_notes":"","sources":[]}`

func TestHuntMode(t *testing.T) {
	r := &fakeRunner{}
	e, gameID, _ := setupAI(t, r)
	base := "/api/v1/games/" + gameID + "/hunt"

	// Sem roadmap ainda.
	h := call[huntResp](t, e, http.MethodGet, base, "", http.StatusOK)
	require.Equal(t, "none", h.Roadmap)
	require.Empty(t, h.Stages)

	r.override = func(claude.Request) (claude.Result, bool) {
		return claude.Result{Structured: []byte(huntRoadmap)}, true
	}
	call[guideResp](t, e, http.MethodPost, "/api/v1/games/"+gameID+"/roadmap", "", http.StatusAccepted)
	require.Empty(t, e.drainAI(t))

	h = call[huntResp](t, e, http.MethodGet, base, "", http.StatusOK)
	require.Equal(t, "ready", h.Roadmap)
	require.Equal(t, []string{"Segredo antes do chefe final"}, h.MissablesOverview)
	require.Len(t, h.Stages, 2)
	// Etapa 1: "venceu" casou com o troféu "Venceu" (já obtido) → completa sozinha.
	require.Len(t, h.Stages[0].TrophyIDs, 1)
	require.Equal(t, 1, h.Stages[0].Earned)
	require.True(t, h.Stages[0].Complete)
	// Etapa 2: "Segredo (perdível)" casou (sem a anotação); o inventado fica como texto.
	require.Len(t, h.Stages[1].TrophyIDs, 1)
	require.Equal(t, []string{"Troféu inventado"}, h.Stages[1].Unmatched)
	require.Zero(t, h.Stages[1].Earned)
	require.False(t, h.Stages[1].Complete)

	// Marcar/desmarcar etapa.
	noContent(t, e, http.MethodPut, base+"/stages/1", `{"done":true}`)
	require.True(t, call[huntResp](t, e, http.MethodGet, base, "", http.StatusOK).Stages[1].Done)
	noContent(t, e, http.MethodPut, base+"/stages/1", `{"done":false}`)
	require.False(t, call[huntResp](t, e, http.MethodGet, base, "", http.StatusOK).Stages[1].Done)
	call[problem](t, e, http.MethodPut, base+"/stages/abc", `{"done":true}`, http.StatusBadRequest)
	call[problem](t, e, http.MethodPut, base+"/stages/0", `{}`, http.StatusBadRequest)

	// Sync deste jogo na fila → syncing; depois de processar, não.
	call[any](t, e, http.MethodPost, "/api/v1/games/"+gameID+"/sync", "", http.StatusAccepted)
	require.True(t, call[huntResp](t, e, http.MethodGet, base, "", http.StatusOK).Syncing)
	e.drain(t)
	require.False(t, call[huntResp](t, e, http.MethodGet, base, "", http.StatusOK).Syncing)

	// Roadmap refeito com menos etapas: a marca da etapa que sumiu não fica órfã.
	noContent(t, e, http.MethodPut, base+"/stages/1", `{"done":true}`)
	r.override = func(claude.Request) (claude.Result, bool) {
		return claude.Result{Structured: []byte(huntRoadmapOneStage)}, true
	}
	call[guideResp](t, e, http.MethodPost, "/api/v1/games/"+gameID+"/roadmap", "", http.StatusAccepted)
	require.Empty(t, e.drainAI(t))
	h = call[huntResp](t, e, http.MethodGet, base, "", http.StatusOK)
	require.Len(t, h.Stages, 1)
	require.Len(t, h.Stages[0].TrophyIDs, 2)
	var n int
	require.NoError(t, e.pool.QueryRow(t.Context(), "SELECT count(*) FROM hunt_progress").Scan(&n))
	require.Zero(t, n)

	// Outra pessoa: o jogo nem existe para ela.
	inviteFriend(t, e)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodGet, base, "", http.StatusNotFound)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodPut, base+"/stages/0", `{"done":true}`, http.StatusNotFound)
}

// noContent: requisição do dono que responde 204 sem corpo.
func noContent(t *testing.T, e *testEnv, method, path, body string) {
	t.Helper()
	resp := e.do(t, method, path, body, true)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}
