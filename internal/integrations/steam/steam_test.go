package steam_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steam"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

// newClient cria um cliente apontando a um servidor fake, com RPS alto para testes rápidos.
func newClient(t *testing.T, h http.Handler) *steam.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return steam.New(steam.Options{BaseURL: srv.URL, APIKey: "FAKEKEY", Lang: "brazilian", RPS: 1000})
}

func serveFixture(t *testing.T, name string) http.HandlerFunc {
	t.Helper()
	body := fixture(t, name)
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func TestGetOwnedGames(t *testing.T) {
	c := newClient(t, serveFixture(t, "owned_games.json"))
	games, err := c.GetOwnedGames(context.Background(), "76561197960000000")
	require.NoError(t, err)
	require.Len(t, games, 2)
	require.Equal(t, 620, games[0].AppID)
	require.Equal(t, "Portal 2", games[0].Name)
	require.Equal(t, 745, games[0].PlaytimeForever)
	require.True(t, games[0].HasCommunityVisibleStats)
	require.False(t, games[1].HasCommunityVisibleStats)
}

func TestGetPlayerAchievements(t *testing.T) {
	c := newClient(t, serveFixture(t, "player_achievements.json"))
	achs, err := c.GetPlayerAchievements(context.Background(), "steamid", 620)
	require.NoError(t, err)
	require.Len(t, achs, 2)
	require.True(t, achs[0].Achieved)
	require.Equal(t, int64(1700000500), achs[0].UnlockTime)
	require.False(t, achs[1].Achieved)
}

func TestGetPlayerAchievementsPrivateProfile(t *testing.T) {
	c := newClient(t, serveFixture(t, "player_achievements_private.json"))
	_, err := c.GetPlayerAchievements(context.Background(), "steamid", 620)
	require.ErrorIs(t, err, steam.ErrProfilePrivate)
}

func TestGetPlayerAchievementsNoStats(t *testing.T) {
	c := newClient(t, serveFixture(t, "player_achievements_no_stats.json"))
	_, err := c.GetPlayerAchievements(context.Background(), "steamid", 620)
	require.ErrorIs(t, err, steam.ErrNoStats)
}

func TestGetSchemaHiddenWithoutDescription(t *testing.T) {
	c := newClient(t, serveFixture(t, "schema.json"))
	name, achs, err := c.GetSchemaForGame(context.Background(), 620)
	require.NoError(t, err)
	require.Equal(t, "Portal 2", name)
	require.Len(t, achs, 2)

	require.False(t, achs[0].Hidden)
	require.NotNil(t, achs[0].Description)
	require.Equal(t, "Complete o jogo.", *achs[0].Description)

	require.True(t, achs[1].Hidden)
	require.Nil(t, achs[1].Description, "oculta sem descrição deve ter Description nil (spoiler guard)")
}

func TestGlobalPercentagesAsString(t *testing.T) {
	c := newClient(t, serveFixture(t, "global_percentages_string.json"))
	pct, err := c.GetGlobalAchievementPercentages(context.Background(), 620)
	require.NoError(t, err)
	require.InDelta(t, 57.4, pct["ACH_WIN_GAME"], 0.001)
	require.InDelta(t, 2.1, pct["ACH_SECRET"], 0.001)
}

func TestGlobalPercentagesAsNumber(t *testing.T) {
	c := newClient(t, serveFixture(t, "global_percentages_number.json"))
	pct, err := c.GetGlobalAchievementPercentages(context.Background(), 620)
	require.NoError(t, err)
	require.InDelta(t, 57.4, pct["ACH_WIN_GAME"], 0.001)
}

func TestRetryOn429ThenSuccess(t *testing.T) {
	var calls atomic.Int32
	body := fixture(t, "owned_games.json")
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write(body)
	}))
	games, err := c.GetOwnedGames(context.Background(), "steamid")
	require.NoError(t, err)
	require.Len(t, games, 2)
	require.Equal(t, int32(2), calls.Load(), "deve ter retentado uma vez após o 429")
}

func TestInvalidKey(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	_, err := c.GetPlayerSummary(context.Background(), "steamid")
	require.ErrorIs(t, err, steam.ErrInvalidKey)
}

func TestResolveVanity(t *testing.T) {
	c := newClient(t, serveFixture(t, "resolve_vanity.json"))
	id, err := c.ResolveVanityURL(context.Background(), "trophyhunter")
	require.NoError(t, err)
	require.Equal(t, "76561197960000000", id)
}

func TestGetPlayerSummaryPublic(t *testing.T) {
	c := newClient(t, serveFixture(t, "player_summary_public.json"))
	p, err := c.GetPlayerSummary(context.Background(), "76561197960000000")
	require.NoError(t, err)
	require.True(t, p.IsPublic())
	require.Equal(t, "TrophyHunter", p.PersonaName)
}

// TestAPIKeyNeverInError garante que a key não vaza em mensagens de erro.
func TestAPIKeyNeverInError(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request with key=FAKEKEY echoed"))
	}))
	_, err := c.GetOwnedGames(context.Background(), "steamid")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "FAKEKEY", "a API key nunca deve aparecer em erros")
}
