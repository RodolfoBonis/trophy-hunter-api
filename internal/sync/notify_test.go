package sync_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/push"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/testsupport"
)

type recordingSender struct {
	mu  sync.Mutex
	got []string
}

func (r *recordingSender) Send(_ context.Context, _ dbgen.PushSubscription, body []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, string(body))
	return http.StatusCreated, nil
}

func (r *recordingSender) titles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

// TestReconnectPushOncePerTransition: a conta PSN expira → um push "Reconecte"; outro sync falhando
// não repete; depois de reconectar, uma nova expiração avisa de novo.
func TestReconnectPushOncePerTransition(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	q := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)
	fake := newFakePSN()
	fake.accessTTL = time.Second
	sender := &recordingSender{}
	notifier := push.New(q, push.Config{PublicKey: "pub", PrivateKey: "priv"}, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	syncer := syncpkg.New(syncpkg.Deps{
		Queries: q, Pool: pool, Queue: jobs, Cipher: newCipher(t), PSN: fake, Notifier: notifier,
		SteamFactory: func(string) syncpkg.SteamAPI { return newFake() },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	_, err := q.UpsertPushSubscription(ctx, dbgen.UpsertPushSubscriptionParams{Endpoint: "https://push/x", P256dh: "k", Auth: "a"})
	require.NoError(t, err)

	integ, err := syncer.ConnectPSN(ctx, syncpkg.ConnectPSNParams{Label: "Principal", NPSSO: "GOOD"})
	require.NoError(t, err)
	processAll(t, jobs, syncer)
	require.Empty(t, sender.titles(), "conta saudável e sync inicial: nada a avisar")

	fake.refreshFails, fake.npssoExpired = true, true
	for i := 0; i < 2; i++ {
		_, _, err = syncer.EnqueuePlatformSync(ctx, integ.ID, syncpkg.ScopeIncremental, syncpkg.TriggerManual)
		require.NoError(t, err)
		processAll(t, jobs, syncer)
	}
	got := sender.titles()
	require.Len(t, got, 1, "um aviso por transição, mesmo com dois syncs falhando")
	require.Contains(t, got[0], "Reconecte a conta PSN (Principal)")
	require.Contains(t, got[0], `"kind":"reconnect"`)

	fake.refreshFails, fake.npssoExpired = false, false
	_, err = syncer.ReconnectPSN(ctx, integ.ID, "NEW")
	require.NoError(t, err)
	processAll(t, jobs, syncer)

	fake.refreshFails, fake.npssoExpired = true, true
	_, _, err = syncer.EnqueuePlatformSync(ctx, integ.ID, syncpkg.ScopeIncremental, syncpkg.TriggerManual)
	require.NoError(t, err)
	processAll(t, jobs, syncer)
	require.Len(t, sender.titles(), 2, "expirou de novo depois de reconectar → avisa de novo")
}
