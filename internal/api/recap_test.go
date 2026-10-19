package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai/claude"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/recap"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

const validRecap = `{"headline":"Semana de Portal 2","highlights":["12 conquistas no Portal 2"],"suggestions":[{"game":"Portal 2","why":"Faltam poucas","trophies":[]}],"encouragement":"Bora fechar!"}`

func TestRecapPeriodAndWeekly(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	r := &fakeRunner{override: func(req claude.Request) (claude.Result, bool) {
		if strings.Contains(req.JSONSchema, `"encouragement"`) {
			return claude.Result{Structured: []byte(validRecap)}, true
		}
		return claude.Result{}, false
	}}
	e := build(t, func(key string) syncpkg.SteamAPI { return accountFake{mu: &mu, keys: &keys, key: key} }, nil, r)
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"K","steam_id_or_vanity":"`+steamMain+`"}`, http.StatusCreated)
	e.drain(t)
	// Traz os desbloqueios para dentro do mês corrente.
	_, err := e.pool.Exec(t.Context(), `UPDATE trophies SET earned_at = now() - interval '1 hour' WHERE earned`)
	require.NoError(t, err)

	month := time.Now().In(recap.Location).Format("2006-01")
	got := call[struct {
		Period        string `json:"period"`
		Label         string `json:"label"`
		Earned        int    `json:"earned"`
		ActiveDays    int    `json:"active_days"`
		LongestStreak int    `json:"longest_streak"`
		TopGames      []struct {
			Title    string `json:"title"`
			Unlocked int    `json:"unlocked"`
		} `json:"top_games"`
	}](t, e, http.MethodGet, "/api/v1/recap?period="+month, "", http.StatusOK)
	require.Equal(t, month, got.Period)
	require.Positive(t, got.Earned)
	require.Equal(t, 1, got.ActiveDays)
	require.Equal(t, 1, got.LongestStreak)
	require.NotEmpty(t, got.TopGames)
	call[problem](t, e, http.MethodGet, "/api/v1/recap?period=2026-13", "", http.StatusBadRequest)

	// Resumo semanal: nada ainda → null; gerar → pendente → ai-worker → pronto + push.
	none := call[struct {
		Recap *json.RawMessage `json:"recap"`
	}](t, e, http.MethodGet, "/api/v1/recap/weekly", "", http.StatusOK)
	require.Nil(t, none.Recap)
	pending := call[struct {
		Recap struct {
			Status string `json:"status"`
		} `json:"recap"`
	}](t, e, http.MethodPost, "/api/v1/recap/weekly", "", http.StatusAccepted)
	require.Equal(t, "pending", pending.Recap.Status)
	require.Empty(t, e.drainAI(t))
	ready := call[struct {
		Recap struct {
			Status  string `json:"status"`
			Content struct {
				Headline string `json:"headline"`
			} `json:"content"`
		} `json:"recap"`
	}](t, e, http.MethodGet, "/api/v1/recap/weekly", "", http.StatusOK)
	require.Equal(t, "ready", ready.Recap.Status)
	require.Equal(t, "Semana de Portal 2", ready.Recap.Content.Headline)
	require.Contains(t, r.last().Prompt, "Portal 2", "o prompt leva os dados da semana")
	var pushes int
	require.NoError(t, e.pool.QueryRow(t.Context(), "SELECT count(*) FROM jobs WHERE kind = 'notify' AND payload->>'kind' = 'weekly_recap'").Scan(&pushes))
	require.Equal(t, 1, pushes)

	// Gerar de novo na mesma semana não cria outro job.
	call[struct{}](t, e, http.MethodPost, "/api/v1/recap/weekly", "", http.StatusAccepted)
	require.Empty(t, e.drainAI(t))
	require.Len(t, r.reqs, 1)
}
