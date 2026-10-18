package push_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/push"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/testsupport"
)

// fakeSender responde por endpoint e guarda as mensagens entregues.
type fakeSender struct {
	mu     sync.Mutex
	status map[string]int
	got    []push.Message
}

func (f *fakeSender) Send(_ context.Context, sub dbgen.PushSubscription, body []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := http.StatusCreated
	if s, ok := f.status[sub.Endpoint]; ok {
		st = s
	}
	if st < 300 {
		var m push.Message
		_ = json.Unmarshal(body, &m)
		f.got = append(f.got, m)
	}
	return st, nil
}

func TestSendHandlesGoneAndFailures(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	q := dbgen.New(pool)
	fake := &fakeSender{status: map[string]int{"https://push/gone": http.StatusGone, "https://push/broken": http.StatusInternalServerError}}
	svc := push.New(q, push.Config{PublicKey: "pub", PrivateKey: "priv"}, fake, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, ep := range []string{"https://push/ok", "https://push/gone", "https://push/broken"} {
		_, err := q.UpsertPushSubscription(ctx, dbgen.UpsertPushSubscriptionParams{Endpoint: ep, P256dh: "k", Auth: "a"})
		require.NoError(t, err)
	}

	res, err := svc.Send(ctx, push.Message{Kind: push.KindTest, Title: "oi"})
	require.NoError(t, err)
	require.Equal(t, push.Result{Sent: 1, Failed: 1, Removed: 1}, res)
	subs, err := q.ListPushSubscriptions(ctx)
	require.NoError(t, err)
	require.Len(t, subs, 2, "410 remove a inscrição")

	// Inscrição que falha 10x seguidas é removida.
	for i := 0; i < 9; i++ {
		_, err = svc.Send(ctx, push.Message{Kind: push.KindTest})
		require.NoError(t, err)
	}
	subs, err = q.ListPushSubscriptions(ctx)
	require.NoError(t, err)
	require.Len(t, subs, 1)
	require.Equal(t, "https://push/ok", subs[0].Endpoint)

	// Preferência desligada: Notify não envia; o teste sempre envia.
	require.NoError(t, push.SavePreferences(ctx, q, push.Preferences{Unlocks: true}))
	fake.got = nil
	res, err = svc.Notify(ctx, push.Message{Kind: push.KindAIReady, Title: "Guia pronto"})
	require.NoError(t, err)
	require.Zero(t, res.Sent)
	_, err = svc.Notify(ctx, push.Message{Kind: push.KindTest, Title: "teste"})
	require.NoError(t, err)
	require.Len(t, fake.got, 1)

	// Sem chaves: desligado, não envia nada.
	off := push.New(q, push.Config{}, fake, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.False(t, off.Enabled())
	res, err = off.Send(ctx, push.Message{Kind: push.KindTest})
	require.NoError(t, err)
	require.Zero(t, res.Sent)
}
