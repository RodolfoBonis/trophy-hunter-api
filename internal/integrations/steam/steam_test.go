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

func TestGetRecentlyPlayedGames(t *testing.T) {
	c := newClient(t, serveFixture(t, "recently_played.json"))
	games, err := c.GetRecentlyPlayedGames(context.Background(), "76561197960000000")
	require.NoError(t, err)
	require.Len(t, games, 2)
	require.Equal(t, 1501750, games[1].AppID)
	require.Equal(t, "Lords of the Fallen", games[1].Name)
	require.Equal(t, 120, games[1].PlaytimeForever)
	require.True(t, games[1].HasCommunityVisibleStats, "desconhecido na API: o sync do jogo descobre")
	require.Zero(t, games[1].RtimeLastPlayed)
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

func TestRetryAfterHonored(t *testing.T) {
	var calls atomic.Int32
	body := fixture(t, "owned_games.json")
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0") // imediato
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write(body)
	}))
	games, err := c.GetOwnedGames(context.Background(), "steamid")
	require.NoError(t, err)
	require.Len(t, games, 2)
	require.Equal(t, int32(2), calls.Load())
}

func TestInvalidKey(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	_, err := c.GetPlayerSummary(context.Background(), "steamid")
	require.ErrorIs(t, err, steam.ErrInvalidKey)
}

// TestForbiddenBodyClassifiedAsPrivate: um 403 com corpo "Profile is not public" é perfil privado,
// NÃO chave inválida (finding 7).
func TestForbiddenBodyClassifiedAsPrivate(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<html><body><h1>Forbidden</h1>Profile is not public</body></html>`))
	}))
	_, err := c.GetPlayerAchievements(context.Background(), "steamid", 620)
	require.ErrorIs(t, err, steam.ErrProfilePrivate)
}

// TestBadRequestBodyClassifiedAsNoStats: um 400 com "Requested app has no stats" vira ErrNoStats.
func TestBadRequestBodyClassifiedAsNoStats(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"playerstats":{"error":"Requested app has no stats"}}`))
	}))
	_, err := c.GetPlayerAchievements(context.Background(), "steamid", 620)
	require.ErrorIs(t, err, steam.ErrNoStats)
}

// TestForbiddenNoBodyIsInvalidKey: 403 sem mensagem de domínio continua sendo chave inválida.
func TestForbiddenNoBodyIsInvalidKey(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`access denied`))
	}))
	_, err := c.GetOwnedGames(context.Background(), "steamid")
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

func TestGetAppAssets(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/IStoreBrowseService/GetItems/v1/", r.URL.Path)
		require.Contains(t, r.URL.Query().Get("input_json"), `"appid":3764200`)
		_, _ = w.Write([]byte(`{"response":{"store_items":[
			{"appid":3764200,"assets":{"asset_url_format":"steam/apps/3764200/${FILENAME}?t=1","library_capsule":"abc/library_capsule.jpg","header":"def/header.jpg"}},
			{"appid":620,"assets":{"asset_url_format":"steam/apps/620/${FILENAME}?t=2","library_capsule":"library_600x900.jpg"}},
			{"appid":999,"assets":{}}
		]}}`))
	}))
	got, err := c.GetAppAssets(context.Background(), []int{3764200, 620, 999})
	require.NoError(t, err)
	require.Equal(t, "https://shared.akamai.steamstatic.com/store_item_assets/steam/apps/3764200/abc/library_capsule.jpg?t=1", got[3764200].LibraryCapsule)
	require.Equal(t, "https://shared.akamai.steamstatic.com/store_item_assets/steam/apps/3764200/def/header.jpg?t=1", got[3764200].Header)
	require.Equal(t, "https://shared.akamai.steamstatic.com/store_item_assets/steam/apps/620/library_600x900.jpg?t=2", got[620].LibraryCapsule)
	require.Empty(t, got[620].Header)
	_, ok := got[999]
	require.False(t, ok, "app sem assets fica fora do mapa")
}
