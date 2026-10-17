package sync_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/psn"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/testsupport"
)

// fakePSN simula a PSN: um título PS5 (Astro Bot) com jogo base + DLC e platina.
type fakePSN struct {
	mu           sync.Mutex
	account      string
	accessTTL    time.Duration
	refreshFails bool
	refreshes    int
	titles       []psn.TrophyTitle
	user         map[string][]psn.UserTrophy
	titleCalls   map[string]int
}

func newFakePSN() *fakePSN {
	updated := time.Date(2026, 10, 12, 21, 47, 0, 0, time.UTC)
	return &fakePSN{
		account:   "4242",
		accessTTL: time.Hour,
		titles: []psn.TrophyTitle{{
			NpCommunicationID: "NPWR1_00", NpServiceName: "trophy2", TrophyTitleName: "Astro Bot",
			TrophyTitleIconURL: "https://img/astro.png", TrophyTitlePlatform: "PS5", LastUpdatedDateTime: updated,
		}},
		user: map[string][]psn.UserTrophy{"NPWR1_00": {
			{TrophyID: 0, Earned: false, TrophyEarnedRate: "3.1"},
			{TrophyID: 1, Earned: true, EarnedDateTime: updated, TrophyEarnedRate: "45.0"},
			{TrophyID: 2, Earned: false, TrophyEarnedRate: "0.8", Progress: "87"},
		}},
		titleCalls: map[string]int{},
	}
}

func (f *fakePSN) tokens(prefix string) psn.Tokens {
	return psn.Tokens{AccessToken: prefix + "-ACC", AccessExpiresAt: time.Now().Add(f.accessTTL), RefreshToken: prefix + "-REF", RefreshExpiresAt: time.Now().Add(60 * 24 * time.Hour)}
}
func (f *fakePSN) ExchangeNPSSO(_ context.Context, npsso string) (psn.Tokens, error) {
	if npsso == "BAD" {
		return psn.Tokens{}, psn.ErrInvalidNPSSO
	}
	return f.tokens(npsso), nil
}
func (f *fakePSN) Refresh(_ context.Context, _ string) (psn.Tokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes++
	if f.refreshFails {
		return psn.Tokens{}, psn.ErrInvalidGrant
	}
	return f.tokens("R"), nil
}
func (f *fakePSN) TrophySummary(context.Context, string) (psn.Summary, error) {
	return psn.Summary{AccountID: f.account, TrophyLevel: 421}, nil
}
func (f *fakePSN) Profile(context.Context, string, string) (psn.Profile, error) {
	return psn.Profile{OnlineID: "rb_lab", Avatar: "https://img/avatar.png"}, nil
}
func (f *fakePSN) TrophyTitles(context.Context, string) ([]psn.TrophyTitle, error) {
	return f.titles, nil
}
func (f *fakePSN) TitleTrophies(_ context.Context, _ string, id, _ string) ([]psn.TrophyDef, error) {
	f.mu.Lock()
	f.titleCalls[id]++
	f.mu.Unlock()
	return []psn.TrophyDef{
		{TrophyID: 0, TrophyType: "platinum", TrophyName: "Platina", TrophyDetail: "Tudo", TrophyGroupID: "default"},
		{TrophyID: 1, TrophyType: "bronze", TrophyName: "Primeiro passo", TrophyDetail: "Comece", TrophyGroupID: "default"},
		{TrophyID: 2, TrophyType: "gold", TrophyName: "Segredo", TrophyHidden: true, TrophyGroupID: "001", TrophyProgressTargetValue: "120"},
	}, nil
}
func (f *fakePSN) TitleGroups(context.Context, string, string, string) ([]psn.TrophyGroup, error) {
	return []psn.TrophyGroup{{TrophyGroupID: "default", TrophyGroupName: "Astro Bot"}, {TrophyGroupID: "001", TrophyGroupName: "Expansão"}}, nil
}
func (f *fakePSN) UserTrophies(_ context.Context, _ string, id, _ string) ([]psn.UserTrophy, error) {
	return f.user[id], nil
}

func newPSNSyncer(t *testing.T, pool *pgxpool.Pool, q *dbgen.Queries, jobs queue.Queue, fake *fakePSN) *syncpkg.Syncer {
	t.Helper()
	return syncpkg.New(syncpkg.Deps{
		Queries: q, Pool: pool, Queue: jobs, Cipher: newCipher(t), PSN: fake,
		SteamFactory: func(string) syncpkg.SteamAPI { return newFake() },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestPSNConnectAndSync(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	q := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)
	fake := newFakePSN()
	syncer := newPSNSyncer(t, pool, q, jobs, fake)

	_, err := syncer.ConnectPSN(ctx, syncpkg.ConnectPSNParams{NPSSO: "BAD"})
	require.ErrorIs(t, err, psn.ErrInvalidNPSSO)

	integ, err := syncer.ConnectPSN(ctx, syncpkg.ConnectPSNParams{NPSSO: "GOOD"})
	require.NoError(t, err)
	require.Equal(t, "rb_lab", integ.Label, "label padrão = online ID")
	require.Equal(t, "4242", integ.AccountID)
	require.True(t, integ.RefreshTokenExpiresAt.Valid)
	_, err = syncer.ConnectPSN(ctx, syncpkg.ConnectPSNParams{NPSSO: "OTHER"})
	require.ErrorIs(t, err, syncpkg.ErrAccountExists)

	processAll(t, jobs, syncer)

	game, err := q.GetGameByExternal(ctx, dbgen.GetGameByExternalParams{IntegrationID: integ.ID, ExternalID: "NPWR1_00"})
	require.NoError(t, err)
	require.Equal(t, "psn", game.Platform)
	require.True(t, game.HasPlatinum)
	require.Equal(t, []string{"PS5"}, game.Platforms)
	require.True(t, game.RemoteUpdatedAt.Valid)

	groups, err := q.ListTrophyGroupsByGame(ctx, game.ID)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	require.True(t, groups[0].IsBase)
	require.Equal(t, "Expansão", groups[1].Name)

	trophies, err := q.ListTrophiesByGame(ctx, game.ID)
	require.NoError(t, err)
	byName := map[string]dbgen.Trophy{}
	for _, tr := range trophies {
		byName[tr.Name] = tr
	}
	require.True(t, byName["Primeiro passo"].Earned)
	require.True(t, byName["Primeiro passo"].EarnedAt.Valid)
	require.InDelta(t, 3.1, *byName["Platina"].RarityPct, 1e-9)
	require.True(t, byName["Segredo"].Hidden)
	require.Equal(t, int32(87), *byName["Segredo"].Progress)
	require.Equal(t, int32(120), *byName["Segredo"].Target)

	after, err := q.GetIntegration(ctx, integ.ID)
	require.NoError(t, err)
	require.True(t, after.InitialSyncDone)
	require.Equal(t, "connected", after.Status)

	// Incremental sem mudança na PSN: nenhum título re-sincronizado.
	before := fake.titleCalls["NPWR1_00"]
	_, enq, err := syncer.EnqueuePlatformSync(ctx, integ.ID, syncpkg.ScopeIncremental, syncpkg.TriggerManual)
	require.NoError(t, err)
	require.True(t, enq)
	processAll(t, jobs, syncer)
	require.Equal(t, before, fake.titleCalls["NPWR1_00"])

	// Novo troféu ganho + lastUpdated mudou → incremental pega e gera unlock_event.
	fake.titles[0].LastUpdatedDateTime = fake.titles[0].LastUpdatedDateTime.Add(time.Hour)
	fake.user["NPWR1_00"][0] = psn.UserTrophy{TrophyID: 0, Earned: true, EarnedDateTime: time.Now().UTC(), TrophyEarnedRate: "3.0"}
	_, _, err = syncer.EnqueuePlatformSync(ctx, integ.ID, syncpkg.ScopeIncremental, syncpkg.TriggerManual)
	require.NoError(t, err)
	processAll(t, jobs, syncer)
	require.Equal(t, 1, countUnlockEvents(t, pool))
}

func TestPSNRefreshAndReconnect(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	q := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)
	fake := newFakePSN()
	fake.accessTTL = time.Second // access "expira" na hora → o sync renova pelo refresh
	syncer := newPSNSyncer(t, pool, q, jobs, fake)

	integ, err := syncer.ConnectPSN(ctx, syncpkg.ConnectPSNParams{Label: "Principal", NPSSO: "GOOD"})
	require.NoError(t, err)
	processAll(t, jobs, syncer)
	require.Positive(t, fake.refreshes, "renovou o access pelo refresh token")

	// Refresh expirado → conta em "reconectar"; jobs morrem (permanente).
	fake.refreshFails = true
	_, _, err = syncer.EnqueuePlatformSync(ctx, integ.ID, syncpkg.ScopeIncremental, syncpkg.TriggerManual)
	require.NoError(t, err)
	processAll(t, jobs, syncer)
	got, err := q.GetIntegration(ctx, integ.ID)
	require.NoError(t, err)
	require.Equal(t, "needs_reconnect", got.Status)

	// NPSSO de outra conta é recusado; o da mesma conta reconecta.
	fake.refreshFails = false
	fake.account = "9999"
	_, err = syncer.ReconnectPSN(ctx, integ.ID, "GOOD")
	require.ErrorIs(t, err, syncpkg.ErrAccountMismatch)
	fake.account = "4242"
	fake.accessTTL = time.Hour
	re, err := syncer.ReconnectPSN(ctx, integ.ID, "NEW")
	require.NoError(t, err)
	require.Equal(t, "connected", re.Status)
}
