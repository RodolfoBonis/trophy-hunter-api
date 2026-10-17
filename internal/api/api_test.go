package api_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/api"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steam"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/auth"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/config"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/crypto"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/testsupport"
)

type fakeSteam struct{}

func (fakeSteam) GetOwnedGames(context.Context, string) ([]steam.OwnedGame, error) {
	return []steam.OwnedGame{{AppID: 620, Name: "Portal 2", PlaytimeForever: 745, RtimeLastPlayed: 1700000000, HasCommunityVisibleStats: true}}, nil
}
func (fakeSteam) GetPlayerAchievements(context.Context, string, int) ([]steam.PlayerAchievement, error) {
	return []steam.PlayerAchievement{
		{APIName: "ACH_WIN_GAME", Achieved: true, UnlockTime: 1700000500},
		{APIName: "ACH_SECRET", Achieved: false},
	}, nil
}
func (fakeSteam) GetSchemaForGame(context.Context, int) (string, []steam.SchemaAchievement, error) {
	d := "Complete o jogo."
	return "Portal 2", []steam.SchemaAchievement{
		{Name: "ACH_WIN_GAME", DisplayName: "Venceu", Description: &d},
		{Name: "ACH_SECRET", DisplayName: "Segredo", Hidden: true},
	}, nil
}
func (fakeSteam) GetGlobalAchievementPercentages(context.Context, int) (map[string]float64, error) {
	return map[string]float64{"ACH_WIN_GAME": 57.4, "ACH_SECRET": 2.1}, nil
}
func (fakeSteam) GetPlayerSummary(context.Context, string) (steam.PlayerSummary, error) {
	return steam.PlayerSummary{SteamID: "76561197960000000", PersonaName: "TH", CommunityVisibleState: 3}, nil
}
func (fakeSteam) GetAppAssets(context.Context, []int) (map[int]steam.AppAssets, error) {
	return nil, nil
}
func (fakeSteam) ResolveVanityURL(context.Context, string) (string, error) {
	return "76561197960000000", nil
}

type testEnv struct {
	srv   *httptest.Server
	q     *dbgen.Queries
	queue queue.Queue
	sync  *syncpkg.Syncer
	ai    *ai.Service
}

func setup(t *testing.T) *testEnv {
	t.Helper()
	return setupWith(t, func(string) syncpkg.SteamAPI { return fakeSteam{} })
}

func setupWith(t *testing.T, factory syncpkg.SteamFactory) *testEnv {
	t.Helper()
	return setupFull(t, factory, nil)
}

func setupFull(t *testing.T, factory syncpkg.SteamFactory, psnAPI syncpkg.PSNAPI) *testEnv {
	t.Helper()
	return build(t, factory, psnAPI, nil)
}

// build monta o ambiente; com runner != nil liga a IA (Redis efêmero + serviço de IA com o runner
// fake, que o teste executa via drainAI).
func build(t *testing.T, factory syncpkg.SteamFactory, psnAPI syncpkg.PSNAPI, runner ai.Runner) *testEnv {
	t.Helper()
	pool := testsupport.NewPostgres(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	k := make([]byte, 32)
	_, _ = rand.Read(k)
	cipher, err := crypto.New(1, k)
	require.NoError(t, err)

	q := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)
	syncer := syncpkg.New(syncpkg.Deps{
		Queries: q, Pool: pool, Queue: jobs, Cipher: cipher,
		SteamFactory: factory,
		PSN:          psnAPI,
		Logger:       logger,
	})

	authn := auth.New(context.Background(), config.Auth{Mode: config.AuthModeDev, RequiredRole: "trophy-hunter-user"}, logger)

	deps := api.HandlerDeps{Queries: q, Pool: pool, Syncer: syncer, Logger: logger}
	var aiSvc *ai.Service
	if runner != nil {
		rdb := testsupport.NewRedis(t)
		aiSvc = ai.New(ai.Deps{Queries: q, Queue: jobs, Redis: rdb, Runner: runner, Workdir: t.TempDir(), Logger: logger})
		deps.AI, deps.Redis = aiSvc, rdb
	}
	handlers := api.NewHandlers(deps)
	router := api.NewRouter(api.RouterDeps{Handlers: handlers, Auth: authn, Logger: logger, ServiceName: "test"})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &testEnv{srv: srv, q: q, queue: jobs, sync: syncer, ai: aiSvc}
}

func (e *testEnv) do(t *testing.T, method, path, body string, auth bool) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, r)
	require.NoError(t, err)
	if auth {
		req.Header.Set("X-Debug-User", "me")
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// drain imita o worker: Claim → HandleJob → Complete/Fail (com lease_token).
func (e *testEnv) drain(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		job, err := e.queue.Claim(ctx, time.Minute, syncpkg.KindSyncPlatform, syncpkg.KindSyncGame)
		require.NoError(t, err)
		if job == nil {
			return
		}
		if herr := e.sync.HandleJob(ctx, *job); herr != nil {
			require.NoError(t, e.queue.Fail(ctx, job.ID, job.LeaseToken, herr))
			continue
		}
		require.NoError(t, e.queue.Complete(ctx, job.ID, job.LeaseToken))
	}
}

func TestMeRequiresAuth(t *testing.T) {
	e := setup(t)
	resp := e.do(t, http.MethodGet, "/api/v1/me", "", false)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))

	resp2 := e.do(t, http.MethodGet, "/api/v1/me", "", true)
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	var u map[string]any
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&u))
	require.Equal(t, "me", u["subject"])
}

func TestConnectSyncAndBrowse(t *testing.T) {
	e := setup(t)

	// Connect Steam.
	resp := e.do(t, http.MethodPost, "/api/v1/integrations/steam",
		`{"label":"Principal","api_key":"FAKEKEY","steam_id_or_vanity":"76561197960000000"}`, true)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	require.NotContains(t, string(body), "FAKEKEY", "nunca retornar a API key")

	// Processa o full sync enfileirado.
	e.drain(t)

	// GET /integrations não vaza segredos.
	ri := e.do(t, http.MethodGet, "/api/v1/integrations", "", true)
	ib, _ := io.ReadAll(ri.Body)
	_ = ri.Body.Close()
	require.Equal(t, http.StatusOK, ri.StatusCode)
	require.Contains(t, string(ib), `"status":"connected"`)
	require.NotContains(t, string(ib), "credentials")

	// GET /games.
	rg := e.do(t, http.MethodGet, "/api/v1/games", "", true)
	gb, _ := io.ReadAll(rg.Body)
	_ = rg.Body.Close()
	require.Equal(t, http.StatusOK, rg.StatusCode)

	var page struct {
		Items []struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Progress struct {
				Total  int `json:"total"`
				Earned int `json:"earned"`
			} `json:"progress"`
		} `json:"items"`
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal(gb, &page))
	require.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	require.Equal(t, "Portal 2", page.Items[0].Title)
	require.Equal(t, 2, page.Items[0].Progress.Total)
	require.Equal(t, 1, page.Items[0].Progress.Earned)

	// GET /games/{id} detalhe.
	rd := e.do(t, http.MethodGet, "/api/v1/games/"+page.Items[0].ID, "", true)
	dbody, _ := io.ReadAll(rd.Body)
	_ = rd.Body.Close()
	require.Equal(t, http.StatusOK, rd.StatusCode)

	var detail struct {
		Progress struct {
			Status string `json:"status"`
			ByType map[string]struct {
				Total  int `json:"total"`
				Earned int `json:"earned"`
			} `json:"by_type"`
		} `json:"progress"`
		Groups []struct {
			IsBase   bool `json:"is_base"`
			Trophies []struct {
				Name        string  `json:"name"`
				Description *string `json:"description"`
				Hidden      bool    `json:"hidden"`
			} `json:"trophies"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(dbody, &detail))
	require.Equal(t, "in_progress", detail.Progress.Status)
	require.Len(t, detail.Groups, 1)
	require.True(t, detail.Groups[0].IsBase)
	require.Len(t, detail.Groups[0].Trophies, 2)
	require.Equal(t, 2, detail.Progress.ByType["achievement"].Total)

	// GET /sync-runs tem o full sync concluído.
	rs := e.do(t, http.MethodGet, "/api/v1/sync-runs?platform=steam", "", true)
	sb, _ := io.ReadAll(rs.Body)
	_ = rs.Body.Close()
	require.Equal(t, http.StatusOK, rs.StatusCode)
	require.Contains(t, string(sb), `"scope":"full"`)
	require.Contains(t, string(sb), `"status":"ok"`)
	require.Contains(t, string(sb), `"games_total":1`)
}

func TestGetUnknownGame404(t *testing.T) {
	e := setup(t)
	resp := e.do(t, http.MethodGet, "/api/v1/games/00000000-0000-0000-0000-000000000000", "", true)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
