package sync_test

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steam"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/crypto"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/testsupport"
)

// fakeSteam é um SteamAPI controlável para os testes de sync.
type fakeSteam struct {
	summary      steam.PlayerSummary
	owned        []steam.OwnedGame
	schema       map[int][]steam.SchemaAchievement
	achievements map[int][]steam.PlayerAchievement
	percentages  map[int]map[string]float64
	achErr       map[int]error // injeta erro em GetPlayerAchievements por appid
}

func (f *fakeSteam) GetOwnedGames(context.Context, string) ([]steam.OwnedGame, error) {
	return f.owned, nil
}
func (f *fakeSteam) GetPlayerAchievements(_ context.Context, _ string, appID int) ([]steam.PlayerAchievement, error) {
	if err := f.achErr[appID]; err != nil {
		return nil, err
	}
	return f.achievements[appID], nil
}
func (f *fakeSteam) GetSchemaForGame(_ context.Context, appID int) (string, []steam.SchemaAchievement, error) {
	return "Game", f.schema[appID], nil
}
func (f *fakeSteam) GetGlobalAchievementPercentages(_ context.Context, appID int) (map[string]float64, error) {
	return f.percentages[appID], nil
}
func (f *fakeSteam) GetPlayerSummary(context.Context, string) (steam.PlayerSummary, error) {
	return f.summary, nil
}
func (f *fakeSteam) ResolveVanityURL(context.Context, string) (string, error) {
	return "76561197960000000", nil
}

func desc(s string) *string { return &s }

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newFake() *fakeSteam {
	return &fakeSteam{
		summary: steam.PlayerSummary{SteamID: "76561197960000000", PersonaName: "TH", AvatarFull: "a", CommunityVisibleState: 3},
		owned: []steam.OwnedGame{
			{AppID: 620, Name: "Portal 2™", PlaytimeForever: 745, RtimeLastPlayed: 1700000000, ImgIconURL: "icon", HasCommunityVisibleStats: true},
		},
		schema: map[int][]steam.SchemaAchievement{
			620: {
				{Name: "ACH_WIN_GAME", DisplayName: "Venceu", Hidden: false, Description: desc("Complete o jogo.")},
				{Name: "ACH_SECRET", DisplayName: "Segredo", Hidden: true, Description: nil},
			},
		},
		achievements: map[int][]steam.PlayerAchievement{
			620: {
				{APIName: "ACH_WIN_GAME", Achieved: true, UnlockTime: 1700000500},
				{APIName: "ACH_SECRET", Achieved: false},
			},
		},
		percentages: map[int]map[string]float64{
			620: {"ACH_WIN_GAME": 57.4, "ACH_SECRET": 2.1},
		},
		achErr: map[int]error{},
	}
}

func newCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	k := make([]byte, 32)
	_, err := rand.Read(k)
	require.NoError(t, err)
	c, err := crypto.New(1, k)
	require.NoError(t, err)
	return c
}

func newSyncer(t *testing.T, pool *pgxpool.Pool, q *dbgen.Queries, jobs queue.Queue, fake syncpkg.SteamAPI) *syncpkg.Syncer {
	t.Helper()
	return syncpkg.New(syncpkg.Deps{
		Queries: q, Pool: pool, Queue: jobs, Cipher: newCipher(t),
		SteamFactory: func(string) syncpkg.SteamAPI { return fake },
		Logger:       testLogger(),
	})
}

// processAll drena a fila imitando o worker: Claim → HandleJob → Complete/Fail. Para quando não há
// mais job pronto (um job que falhou com backoff tem run_at no futuro e não é reivindicado).
func processAll(t *testing.T, q queue.Queue, s *syncpkg.Syncer) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		job, err := q.Claim(ctx, time.Minute)
		require.NoError(t, err)
		if job == nil {
			return
		}
		herr := s.HandleJob(ctx, *job)
		if herr != nil {
			require.NoError(t, q.Fail(ctx, job.ID, job.LeaseToken, herr))
			continue
		}
		require.NoError(t, q.Complete(ctx, job.ID, job.LeaseToken))
	}
	t.Fatal("fila não drenou (possível loop)")
}

func countUnlockEvents(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), "SELECT count(*) FROM unlock_events").Scan(&n))
	return n
}

func latestRun(t *testing.T, q *dbgen.Queries) dbgen.SyncRun {
	t.Helper()
	runs, err := q.ListSyncRuns(context.Background(), dbgen.ListSyncRunsParams{Limit: 1})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	return runs[0]
}

func TestFullSyncThenIncrementalDiff(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	q := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)
	fake := newFake()
	syncer := newSyncer(t, pool, q, jobs, fake)

	integ, err := syncer.ConnectSteam(ctx, syncpkg.ConnectSteamParams{APIKey: "FAKEKEY", IDOrVanity: "76561197960000000"})
	require.NoError(t, err)
	require.Equal(t, "connected", integ.Status)
	require.Equal(t, "TH", integ.DisplayName)

	processAll(t, jobs, syncer)

	game, err := q.GetGameByExternal(ctx, dbgen.GetGameByExternalParams{IntegrationID: integ.ID, ExternalID: "620"})
	require.NoError(t, err)
	require.True(t, game.HasAchievements)
	require.Equal(t, "portal 2", game.NormalizedTitle) // ™ removido
	require.True(t, game.SchemaSyncedAt.Valid)
	require.True(t, game.PercentagesSyncedAt.Valid)
	require.True(t, game.RemoteUpdatedAt.Valid, "marca d'água gravada após sucesso")

	trophies, err := q.ListTrophiesByGame(ctx, game.ID)
	require.NoError(t, err)
	require.Len(t, trophies, 2)
	byExt := map[string]dbgen.Trophy{}
	for _, tr := range trophies {
		byExt[tr.ExternalID] = tr
	}
	require.True(t, byExt["ACH_WIN_GAME"].Earned)
	require.NotNil(t, byExt["ACH_WIN_GAME"].RarityPct)
	require.InDelta(t, 57.4, *byExt["ACH_WIN_GAME"].RarityPct, 0.01)
	require.False(t, byExt["ACH_SECRET"].Earned)
	require.True(t, byExt["ACH_SECRET"].Hidden)
	require.Nil(t, byExt["ACH_SECRET"].Description, "oculta sem descrição → nil")

	// Sync inicial NÃO gera unlock_events.
	require.Equal(t, 0, countUnlockEvents(t, pool))

	// Run pai fechou ok, com 1 jogo total e 0 falhas.
	run := latestRun(t, q)
	require.Equal(t, "full", run.Scope)
	require.Equal(t, "ok", run.Status)
	require.Equal(t, int32(1), run.GamesTotal)
	require.Equal(t, int32(1), run.GamesDone)
	require.Equal(t, int32(0), run.GamesFailed)

	integAfter, err := q.GetIntegration(ctx, integ.ID)
	require.NoError(t, err)
	require.True(t, integAfter.InitialSyncDone)
	require.True(t, integAfter.LastSyncedAt.Valid)

	// --- Incremental: o jogador desbloqueia ACH_SECRET e o jogo muda (playtime/rtime) ---
	fake.owned[0].PlaytimeForever = 800
	fake.owned[0].RtimeLastPlayed = 1700009999
	fake.achievements[620] = []steam.PlayerAchievement{
		{APIName: "ACH_WIN_GAME", Achieved: true, UnlockTime: 1700000500},
		{APIName: "ACH_SECRET", Achieved: true, UnlockTime: 1700010000},
	}

	_, enq, err := syncer.EnqueuePlatformSync(ctx, integ.ID, syncpkg.ScopeIncremental, syncpkg.TriggerManual)
	require.NoError(t, err)
	require.True(t, enq)
	processAll(t, jobs, syncer)

	// Agora há exatamente 1 unlock_event (a transição de ACH_SECRET).
	require.Equal(t, 1, countUnlockEvents(t, pool))

	trophies2, err := q.ListTrophiesByGame(ctx, game.ID)
	require.NoError(t, err)
	for _, tr := range trophies2 {
		require.True(t, tr.Earned, "ambos troféus desbloqueados")
	}

	rows, err := q.ListGames(ctx, dbgen.ListGamesParams{Sort: "last_played", Lim: 50, Off: 0, Almost: 0.8})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(2), rows[0].Total)
	require.Equal(t, int64(2), rows[0].Earned)
	require.Equal(t, "completed", rows[0].StatusBucket)
}

// TestFanOutCountsFailedChild cobre o fan-out com DOIS jogos: um sincroniza e o outro falha de
// forma permanente (chave inválida). O run fecha com games_failed=1 e a integração vai a 'error'.
func TestFanOutCountsFailedChild(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	q := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)

	fake := newFake()
	fake.owned = append(fake.owned, steam.OwnedGame{
		AppID: 630, Name: "Outro Jogo", PlaytimeForever: 10, RtimeLastPlayed: 1700000001, HasCommunityVisibleStats: true,
	})
	fake.schema[630] = []steam.SchemaAchievement{{Name: "A1", DisplayName: "Um", Description: desc("x")}}
	fake.achErr[630] = steam.ErrInvalidKey // falha permanente ao buscar conquistas desse jogo

	syncer := newSyncer(t, pool, q, jobs, fake)

	integ0, err := syncer.ConnectSteam(ctx, syncpkg.ConnectSteamParams{APIKey: "FAKEKEY", IDOrVanity: "76561197960000000"})
	require.NoError(t, err)
	processAll(t, jobs, syncer)

	run := latestRun(t, q)
	require.Equal(t, "full", run.Scope)
	require.Equal(t, int32(2), run.GamesTotal)
	require.Equal(t, int32(2), run.GamesDone)
	require.Equal(t, int32(1), run.GamesFailed)
	require.Equal(t, "error", run.Status)

	// Jogo que falhou não ganhou marca d'água (será re-tentado no próximo incremental).
	failed, err := q.GetGameByExternal(ctx, dbgen.GetGameByExternalParams{IntegrationID: integ0.ID, ExternalID: "630"})
	require.NoError(t, err)
	require.False(t, failed.RemoteUpdatedAt.Valid, "jogo falho não grava marca d'água")

	// Jogo que teve sucesso ganhou a marca d'água.
	ok, err := q.GetGameByExternal(ctx, dbgen.GetGameByExternalParams{IntegrationID: integ0.ID, ExternalID: "620"})
	require.NoError(t, err)
	require.True(t, ok.RemoteUpdatedAt.Valid)

	// Erro permanente (chave inválida) → integração em 'error'; initial_sync_done NÃO ligado.
	integ, err := q.GetIntegration(ctx, integ0.ID)
	require.NoError(t, err)
	require.Equal(t, "error", integ.Status)
	require.False(t, integ.InitialSyncDone, "full com falhas não marca initial_sync_done")
}

// TestPrivateEmptyLibrary cobre o finding 6: perfil privado devolve biblioteca vazia; não marcar
// initial_sync_done nem connected — a integração fica error_private.
func TestPrivateEmptyLibrary(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	q := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)

	fake := newFake()
	fake.owned = nil                       // biblioteca "vazia" (resposta {"response":{}})
	fake.summary.CommunityVisibleState = 1 // privado
	syncer := newSyncer(t, pool, q, jobs, fake)

	integ, err := syncer.ConnectSteam(ctx, syncpkg.ConnectSteamParams{APIKey: "FAKEKEY", IDOrVanity: "76561197960000000"})
	require.NoError(t, err)
	require.Equal(t, "error_private", integ.Status) // já no connect
	processAll(t, jobs, syncer)

	after, err := q.GetIntegration(ctx, integ.ID)
	require.NoError(t, err)
	require.Equal(t, "error_private", after.Status, "não pode virar connected com perfil privado")
	require.False(t, after.InitialSyncDone, "não marca initial_sync_done com biblioteca vazia privada")

	run := latestRun(t, q)
	require.Equal(t, "error", run.Status)
}

// TestUnlockEventUniqueness garante que o índice único (trophy_id) + ON CONFLICT DO NOTHING impede
// eventos duplicados para o mesmo troféu (finding 18).
func TestUnlockEventUniqueness(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	q := dbgen.New(pool)

	integ, err := q.CreateIntegration(ctx, dbgen.CreateIntegrationParams{
		Platform: "steam", Label: "Principal", AccountID: "76561197960000001",
	})
	require.NoError(t, err)
	game, err := q.UpsertGameIdentity(ctx, dbgen.UpsertGameIdentityParams{
		IntegrationID: integ.ID, Platform: "steam", ExternalID: "999", Title: "T", NormalizedTitle: "t",
		Platforms: []string{}, HasAchievements: true,
	})
	require.NoError(t, err)
	grp, err := q.UpsertTrophyGroup(ctx, dbgen.UpsertTrophyGroupParams{
		GameID: game.ID, ExternalID: "default", Name: "Base", IsBase: true,
	})
	require.NoError(t, err)
	tr, err := q.UpsertTrophyDefinition(ctx, dbgen.UpsertTrophyDefinitionParams{
		GameID: game.ID, GroupID: grp.ID, ExternalID: "A1", Name: "Um", Type: "achievement",
	})
	require.NoError(t, err)

	require.NoError(t, q.InsertUnlockEvent(ctx, tr.ID))
	require.NoError(t, q.InsertUnlockEvent(ctx, tr.ID)) // 2ª vez: no-op
	require.Equal(t, 1, countUnlockEvents(t, pool))
}

func TestConnectPrivateProfileMarksErrorPrivate(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	q := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)
	fake := newFake()
	fake.summary.CommunityVisibleState = 1 // privado

	syncer := newSyncer(t, pool, q, jobs, fake)

	integ, err := syncer.ConnectSteam(ctx, syncpkg.ConnectSteamParams{APIKey: "FAKEKEY", IDOrVanity: "76561197960000000"})
	require.NoError(t, err)
	require.Equal(t, "error_private", integ.Status)
}
