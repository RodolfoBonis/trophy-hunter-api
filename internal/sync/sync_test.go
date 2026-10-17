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
}

func (f *fakeSteam) GetOwnedGames(context.Context, string) ([]steam.OwnedGame, error) {
	return f.owned, nil
}
func (f *fakeSteam) GetPlayerAchievements(_ context.Context, _ string, appID int) ([]steam.PlayerAchievement, error) {
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

func drainJobs(t *testing.T, q queue.Queue, s *syncpkg.Syncer) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		job, err := q.Claim(ctx, time.Minute)
		require.NoError(t, err)
		if job == nil {
			return
		}
		require.NoError(t, s.HandleJob(ctx, *job))
		require.NoError(t, q.Complete(ctx, job.ID))
	}
	t.Fatal("jobs não drenaram")
}

func countUnlockEvents(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), "SELECT count(*) FROM unlock_events").Scan(&n))
	return n
}

func TestFullSyncThenIncrementalDiff(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	q := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)
	fake := newFake()
	syncer := syncpkg.New(syncpkg.Deps{
		Queries: q, Pool: pool, Queue: jobs, Cipher: newCipher(t),
		SteamFactory: func(string) syncpkg.SteamAPI { return fake },
		Logger:       testLogger(),
	})

	// Connect → grava integração e enfileira full sync.
	integ, err := syncer.ConnectSteam(ctx, "FAKEKEY", "76561197960000000")
	require.NoError(t, err)
	require.Equal(t, "connected", integ.Status)
	require.Equal(t, "TH", integ.DisplayName)

	// Processa o full sync.
	drainJobs(t, jobs, syncer)

	// Jogo + troféus populados.
	game, err := q.GetGameByExternal(ctx, dbgen.GetGameByExternalParams{Platform: "steam", ExternalID: "620"})
	require.NoError(t, err)
	require.True(t, game.HasAchievements)
	require.Equal(t, "portal 2", game.NormalizedTitle) // ™ removido
	require.True(t, game.SchemaSyncedAt.Valid)
	require.True(t, game.PercentagesSyncedAt.Valid)

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

	// Integração marcada como initial_sync_done.
	integAfter, err := q.GetIntegration(ctx, "steam")
	require.NoError(t, err)
	require.True(t, integAfter.InitialSyncDone)
	require.True(t, integAfter.LastSyncedAt.Valid)

	// --- Incremental: o jogador desbloqueia ACH_SECRET e o jogo muda (playtime) ---
	fake.owned[0].PlaytimeForever = 800
	fake.owned[0].RtimeLastPlayed = 1700009999
	fake.achievements[620] = []steam.PlayerAchievement{
		{APIName: "ACH_WIN_GAME", Achieved: true, UnlockTime: 1700000500},
		{APIName: "ACH_SECRET", Achieved: true, UnlockTime: 1700010000},
	}

	_, enq, err := syncer.EnqueuePlatformSync(ctx, "steam", syncpkg.ScopeIncremental, syncpkg.TriggerManual)
	require.NoError(t, err)
	require.True(t, enq)
	drainJobs(t, jobs, syncer)

	// Agora há exatamente 1 unlock_event (a transição de ACH_SECRET).
	require.Equal(t, 1, countUnlockEvents(t, pool))

	trophies2, err := q.ListTrophiesByGame(ctx, game.ID)
	require.NoError(t, err)
	for _, tr := range trophies2 {
		require.True(t, tr.Earned, "ambos troféus desbloqueados")
	}

	// ListGames reflete 100% → status completed.
	rows, err := q.ListGames(ctx, dbgen.ListGamesParams{Sort: "last_played", Lim: 50, Off: 0, Almost: 0.8})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(2), rows[0].Total)
	require.Equal(t, int64(2), rows[0].Earned)
	require.Equal(t, "completed", rows[0].StatusBucket)
}

func TestConnectPrivateProfileMarksErrorPrivate(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	q := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)
	fake := newFake()
	fake.summary.CommunityVisibleState = 1 // privado

	syncer := syncpkg.New(syncpkg.Deps{
		Queries: q, Pool: pool, Queue: jobs, Cipher: newCipher(t),
		SteamFactory: func(string) syncpkg.SteamAPI { return fake },
		Logger:       testLogger(),
	})

	integ, err := syncer.ConnectSteam(ctx, "FAKEKEY", "76561197960000000")
	require.NoError(t, err)
	require.Equal(t, "error_private", integ.Status)
}
