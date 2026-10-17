package api_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

// TestInsights usa as duas contas do accountFake: Principal (Portal 2 1/2, Portal 0/2) e Alt
// (Portal 2 2/2). Raridades do schema: A = 50% (comum), B = 10% (raro).
func TestInsights(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	e := setupWith(t, func(key string) syncpkg.SteamAPI { return accountFake{mu: &mu, keys: &keys, key: key} })
	main := call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam",
		`{"label":"Principal","api_key":"K","steam_id_or_vanity":"`+steamMain+`"}`, http.StatusCreated)
	alt := call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam",
		`{"label":"Alt","steam_id_or_vanity":"`+steamAlt+`"}`, http.StatusCreated)
	e.drain(t)

	stats := call[struct {
		Games struct {
			Total      int `json:"total"`
			Completed  int `json:"completed"`
			NotStarted int `json:"not_started"`
		} `json:"games"`
		Trophies struct {
			Total, Earned int
			Rarity        struct{ Rare, Common, Epic int } `json:"rarity"`
		} `json:"trophies"`
		Completions []struct{ Title string } `json:"completions"`
		Evolution   []struct {
			Month              string
			Earned, Cumulative int
		} `json:"evolution"`
		Rarest []struct {
			Name      string
			RarityPct float64 `json:"rarity_pct"`
		} `json:"rarest"`
	}](t, e, http.MethodGet, "/api/v1/stats", "", http.StatusOK)
	// Jogo conta uma vez, pela conta mais avançada: Portal 2 (Alt, 2/2) + Portal (Principal, 0/2).
	require.Equal(t, 2, stats.Games.Total)
	require.Equal(t, 1, stats.Games.Completed)
	require.Equal(t, 1, stats.Games.NotStarted)
	require.Equal(t, 4, stats.Trophies.Total)
	require.Equal(t, 2, stats.Trophies.Earned)
	require.Equal(t, 1, stats.Trophies.Rarity.Rare)
	require.Equal(t, 1, stats.Trophies.Rarity.Common)
	require.Len(t, stats.Completions, 1)
	require.Equal(t, "Portal 2", stats.Completions[0].Title)
	require.Equal(t, "B", stats.Rarest[0].Name, "mais rara primeiro")
	require.NotEmpty(t, stats.Evolution)
	require.Equal(t, 2, stats.Evolution[len(stats.Evolution)-1].Cumulative)

	// Linha do tempo: todas as contas (1 da Principal + 2 da Alt), com cursor.
	type item struct {
		Name          string `json:"name"`
		AccountLabel  string `json:"account_label"`
		CompletesGame bool   `json:"completes_game"`
	}
	type page struct {
		Items      []item  `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	p1 := call[page](t, e, http.MethodGet, "/api/v1/timeline?limit=2", "", http.StatusOK)
	require.Len(t, p1.Items, 2)
	require.Equal(t, "Alt", p1.Items[0].AccountLabel)
	require.True(t, p1.Items[0].CompletesGame, "o último desbloqueio da Alt fechou Portal 2")
	require.NotNil(t, p1.NextCursor)
	p2 := call[page](t, e, http.MethodGet, "/api/v1/timeline?limit=2&cursor="+urlEscape(*p1.NextCursor), "", http.StatusOK)
	require.Len(t, p2.Items, 1)
	require.Equal(t, "Principal", p2.Items[0].AccountLabel)
	require.Nil(t, p2.NextCursor)
	bad := e.do(t, http.MethodGet, "/api/v1/timeline?cursor=xyz", "", true)
	_ = bad.Body.Close()
	require.Equal(t, http.StatusBadRequest, bad.StatusCode)

	// Próxima Platina: com a Alt, Portal 2 está completo e Portal não começou → fila vazia.
	type next struct {
		Items []struct {
			Title     string  `json:"title"`
			Remaining int     `json:"remaining"`
			Score     float64 `json:"score"`
		} `json:"items"`
		Today *struct {
			GameID   string                  `json:"game_id"`
			Trophies []struct{ Name string } `json:"trophies"`
		} `json:"today"`
	}
	n := call[next](t, e, http.MethodGet, "/api/v1/next", "", http.StatusOK)
	require.Empty(t, n.Items)
	require.Nil(t, n.Today)

	// Sem a Alt, Portal 2 (Principal, 1/2) entra na fila e vira o "hoje".
	del := e.do(t, http.MethodDelete, "/api/v1/integrations/"+alt.ID, "", true)
	_ = del.Body.Close()
	n = call[next](t, e, http.MethodGet, "/api/v1/next", "", http.StatusOK)
	require.Len(t, n.Items, 1)
	require.Equal(t, "Portal 2", n.Items[0].Title)
	require.Equal(t, 1, n.Items[0].Remaining)
	require.Greater(t, n.Items[0].Score, 0.0)
	require.NotNil(t, n.Today)
	require.Equal(t, "B", n.Today.Trophies[0].Name)

	// Marcações: abandonar tira da fila; tag perdível no troféu vira alerta.
	lib := call[struct {
		Items []struct {
			ID         string `json:"id"`
			ExternalID string `json:"external_id"`
		} `json:"items"`
	}](t, e, http.MethodGet, "/api/v1/games?sort=title", "", http.StatusOK)
	var portal2 string
	for _, it := range lib.Items {
		if it.ExternalID == "620" {
			portal2 = it.ID
		}
	}
	require.NotEmpty(t, portal2)

	game := call[struct {
		Abandoned bool     `json:"abandoned"`
		Priority  int      `json:"priority"`
		Tags      []string `json:"tags"`
	}](t, e, http.MethodPatch, "/api/v1/games/"+portal2, `{"abandoned":true,"priority":2,"tags":["grind","grind"]}`, http.StatusOK)
	require.True(t, game.Abandoned)
	require.Equal(t, 2, game.Priority)
	require.Equal(t, []string{"grind"}, game.Tags, "tags deduplicadas")
	require.Empty(t, call[next](t, e, http.MethodGet, "/api/v1/next", "", http.StatusOK).Items)

	invalid := e.do(t, http.MethodPatch, "/api/v1/games/"+portal2, `{"tags":["xyz"],"priority":7}`, true)
	_ = invalid.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, invalid.StatusCode)

	detail := call[struct {
		Groups []struct {
			Trophies []struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Earned bool   `json:"earned"`
			} `json:"trophies"`
		} `json:"groups"`
	}](t, e, http.MethodGet, "/api/v1/games/"+portal2, "", http.StatusOK)
	var trophyB string
	for _, tr := range detail.Groups[0].Trophies {
		if tr.Name == "B" {
			trophyB = tr.ID
		}
	}
	call[struct{}](t, e, http.MethodPatch, "/api/v1/games/"+portal2, `{"abandoned":false}`, http.StatusOK)
	tr := call[struct {
		Tags               []string `json:"tags"`
		ServerShutdownRisk bool     `json:"server_shutdown_risk"`
	}](t, e, http.MethodPatch, "/api/v1/trophies/"+trophyB, `{"tags":["missable","online"],"server_shutdown_risk":true}`, http.StatusOK)
	require.Equal(t, []string{"missable", "online"}, tr.Tags)
	require.True(t, tr.ServerShutdownRisk)

	alerts := call[[]struct {
		Name      string   `json:"name"`
		GameTitle string   `json:"game_title"`
		Tags      []string `json:"tags"`
	}](t, e, http.MethodGet, "/api/v1/alerts", "", http.StatusOK)
	require.Len(t, alerts, 1)
	require.Equal(t, "B", alerts[0].Name)
	require.Equal(t, "Portal 2", alerts[0].GameTitle)

	// Urgência online sobe a nota do jogo na fila.
	n = call[next](t, e, http.MethodGet, "/api/v1/next", "", http.StatusOK)
	require.Len(t, n.Items, 1)
	_ = main
}
