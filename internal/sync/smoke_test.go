package sync_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steam"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/testsupport"
)

// fakeSteamServer emula os endpoints da Steam Web API usados no sync, servindo JSON por path.
func fakeSteamServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("/ISteamUser/GetPlayerSummaries/v2/", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"response":{"players":[{"steamid":"76561197960000000","personaname":"TH","avatarfull":"http://a/f.jpg","communityvisibilitystate":3}]}}`)
	})
	mux.HandleFunc("/IPlayerService/GetOwnedGames/v1/", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"response":{"game_count":1,"games":[{"appid":620,"name":"Portal 2","playtime_forever":745,"rtime_last_played":1700000000,"img_icon_url":"ic","has_community_visible_stats":true}]}}`)
	})
	mux.HandleFunc("/ISteamUserStats/GetSchemaForGame/v2/", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"game":{"gameName":"Portal 2","availableGameStats":{"achievements":[{"name":"ACH_WIN_GAME","displayName":"Venceu","hidden":0,"description":"Complete.","icon":"http://i/w.jpg","icongray":"http://i/wg.jpg"},{"name":"ACH_SECRET","displayName":"Segredo","hidden":1,"icon":"http://i/s.jpg","icongray":"http://i/sg.jpg"}]}}}`)
	})
	mux.HandleFunc("/ISteamUserStats/GetPlayerAchievements/v1/", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"playerstats":{"steamID":"76561197960000000","gameName":"Portal 2","success":true,"achievements":[{"apiname":"ACH_WIN_GAME","achieved":1,"unlocktime":1700000500},{"apiname":"ACH_SECRET","achieved":0,"unlocktime":0}]}}`)
	})
	mux.HandleFunc("/ISteamUserStats/GetGlobalAchievementPercentagesForApp/v2/", func(w http.ResponseWriter, _ *http.Request) {
		// percent como string (caso real da Steam).
		write(w, `{"achievementpercentages":{"achievements":[{"name":"ACH_WIN_GAME","percent":"57.4"},{"name":"ACH_SECRET","percent":"2.1"}]}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestSmokeEndToEndRealClient prova o caminho completo com o cliente Steam REAL (HTTP) via
// STEAM_API_BASE_URL apontando a um servidor fake: connect → full sync → troféus no banco.
func TestSmokeEndToEndRealClient(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	q := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)

	fakeAPI := fakeSteamServer(t)
	factory := func(apiKey string) syncpkg.SteamAPI {
		return steam.New(steam.Options{BaseURL: fakeAPI.URL, APIKey: apiKey, Lang: "brazilian", RPS: 1000, Logger: testLogger()})
	}
	syncer := syncpkg.New(syncpkg.Deps{
		Queries: q, Pool: pool, Queue: jobs, Cipher: newCipher(t),
		SteamFactory: factory, Logger: testLogger(),
	})

	integ, err := syncer.ConnectSteam(ctx, syncpkg.ConnectSteamParams{UserID: ownerID, APIKey: "REALISH_KEY", IDOrVanity: "76561197960000000"})
	require.NoError(t, err)
	require.Equal(t, "connected", integ.Status)

	processAll(t, jobs, syncer)

	game, err := q.GetGameByExternal(ctx, dbgen.GetGameByExternalParams{IntegrationID: integ.ID, ExternalID: "620"})
	require.NoError(t, err)
	require.True(t, game.HasAchievements)
	require.True(t, strings.Contains(game.Cover, "library_600x900.jpg"))

	trophies, err := q.ListTrophiesByGame(ctx, game.ID)
	require.NoError(t, err)
	require.Len(t, trophies, 2)

	rows, err := q.ListGames(ctx, dbgen.ListGamesParams{UserID: ownerID, Sort: "last_played", Lim: 50, Off: 0, Almost: 0.8})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(2), rows[0].Total)
	require.Equal(t, int64(1), rows[0].Earned)
	require.Equal(t, "in_progress", rows[0].StatusBucket)
}
