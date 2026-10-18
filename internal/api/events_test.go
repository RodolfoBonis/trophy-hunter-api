package api_test

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steam"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

// unlockingSteam é o fakeSteam em que a conquista oculta passa a estar obtida depois de `unlocked`.
type unlockingSteam struct {
	fakeSteam
	unlocked *atomic.Bool
}

func (f unlockingSteam) GetPlayerAchievements(ctx context.Context, id string, app int) ([]steam.PlayerAchievement, error) {
	a, err := f.fakeSteam.GetPlayerAchievements(ctx, id, app)
	if f.unlocked.Load() {
		a[1] = steam.PlayerAchievement{APIName: "ACH_SECRET", Achieved: true, UnlockTime: 1700009999}
	}
	return a, err
}

func (f unlockingSteam) GetOwnedGames(ctx context.Context, id string) ([]steam.OwnedGame, error) {
	g, err := f.fakeSteam.GetOwnedGames(ctx, id)
	if f.unlocked.Load() {
		g[0].RtimeLastPlayed++ // jogou de novo → o incremental refaz o jogo
		g[0].PlaytimeForever++
	}
	return g, err
}

type unlockEvent struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	GameTitle     string  `json:"game_title"`
	RarityPct     float64 `json:"rarity_pct"`
	CompletesGame bool    `json:"completes_game"`
}

func TestUnlockEventsFeedTheToasts(t *testing.T) {
	var unlocked atomic.Bool
	e := setupWith(t, func(string) syncpkg.SteamAPI { return unlockingSteam{unlocked: &unlocked} })
	integ := call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"K","steam_id_or_vanity":"76561197960000000"}`, http.StatusCreated)
	e.drain(t)

	none := call[[]unlockEvent](t, e, http.MethodGet, "/api/v1/events/unseen", "", http.StatusOK)
	require.Empty(t, none, "o sync inicial não gera avisos (seria a biblioteca inteira)")

	unlocked.Store(true)
	call[struct{}](t, e, http.MethodPost, "/api/v1/integrations/"+integ.ID+"/sync", `{"scope":"incremental"}`, http.StatusAccepted)
	e.drain(t)

	evs := call[[]unlockEvent](t, e, http.MethodGet, "/api/v1/events/unseen", "", http.StatusOK)
	require.Len(t, evs, 1)
	require.Equal(t, "Segredo", evs[0].Name)
	require.Equal(t, "Portal 2", evs[0].GameTitle)
	require.InDelta(t, 2.1, evs[0].RarityPct, 1e-9)
	require.True(t, evs[0].CompletesGame, "com ela o jogo fica 100% → cerimônia")

	call[struct{}](t, e, http.MethodPost, "/api/v1/events/seen", `{"ids":["not-a-uuid"]}`, http.StatusUnprocessableEntity)
	call[struct{}](t, e, http.MethodPost, "/api/v1/events/seen", `{"ids":[]}`, http.StatusUnprocessableEntity)
	res := e.do(t, http.MethodPost, "/api/v1/events/seen", `{"ids":["`+evs[0].ID+`"]}`, true)
	_ = res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	require.Empty(t, call[[]unlockEvent](t, e, http.MethodGet, "/api/v1/events/unseen", "", http.StatusOK))
}

func TestHTTPMetricsUseRoutePattern(t *testing.T) {
	e := setup(t)
	res := e.do(t, http.MethodGet, "/api/v1/games/00000000-0000-0000-0000-000000000001", "", true)
	_ = res.Body.Close()
	m := e.do(t, http.MethodGet, "/metrics", "", false)
	defer func() { _ = m.Body.Close() }()
	body, _ := io.ReadAll(m.Body)
	require.Contains(t, string(body), `route="/api/v1/games/{id}"`, "rota pelo padrão do chi, não pelo id")
	require.NotContains(t, string(body), "00000000-0000-0000-0000-000000000001")
}
