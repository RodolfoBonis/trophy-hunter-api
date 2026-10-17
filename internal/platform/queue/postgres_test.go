package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/testsupport"
)

func TestEnqueueClaimComplete(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := queue.NewPostgres(pool, queue.DefaultBackoff)
	ctx := context.Background()

	job, enq, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_platform", Payload: map[string]string{"platform": "steam"}})
	require.NoError(t, err)
	require.True(t, enq)
	require.NotEmpty(t, job.ID)

	claimed, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	require.Equal(t, 1, claimed.Attempts)
	require.JSONEq(t, `{"platform":"steam"}`, string(claimed.Payload))

	// Nada mais elegível (o único job está running).
	none, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.Nil(t, none)

	require.NoError(t, q.Complete(ctx, claimed.ID))
	after, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.Nil(t, after)
}

func TestDedupeSuppressesDuplicate(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := queue.NewPostgres(pool, queue.DefaultBackoff)
	ctx := context.Background()

	_, enq1, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game", DedupeKey: "game:1"})
	require.NoError(t, err)
	require.True(t, enq1)

	job2, enq2, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game", DedupeKey: "game:1"})
	require.NoError(t, err)
	require.False(t, enq2, "segundo enqueue idêntico deve ser suprimido")
	require.Nil(t, job2)

	// Após concluir, um novo enqueue com a mesma chave é permitido.
	claimed, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NoError(t, q.Complete(ctx, claimed.ID))

	_, enq3, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game", DedupeKey: "game:1"})
	require.NoError(t, err)
	require.True(t, enq3)
}

func TestFailRetriesThenDead(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	// Backoff mínimo para o job voltar a ficar elegível de imediato no teste.
	q := queue.NewPostgres(pool, queue.BackoffConfig{Base: time.Nanosecond, Max: time.Nanosecond, Jitter: 0})
	ctx := context.Background()

	_, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game", MaxAttempts: 2})
	require.NoError(t, err)

	// Tentativa 1 → falha → reagenda.
	c1, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, c1)
	require.NoError(t, q.Fail(ctx, c1.ID, errors.New("boom")))

	time.Sleep(10 * time.Millisecond)

	// Tentativa 2 (== max) → falha → dead.
	c2, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, c2)
	require.Equal(t, 2, c2.Attempts)
	require.NoError(t, q.Fail(ctx, c2.ID, errors.New("boom again")))

	// Não deve mais ser reivindicável.
	time.Sleep(10 * time.Millisecond)
	dead, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.Nil(t, dead)

	var status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, c2.ID).Scan(&status))
	require.Equal(t, "dead", status)
}

func TestClaimReclaimsExpiredLease(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := queue.NewPostgres(pool, queue.DefaultBackoff)
	ctx := context.Background()

	_, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game"})
	require.NoError(t, err)

	// Lease curtíssimo: expira antes do próximo claim.
	c1, err := q.Claim(ctx, 1*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, c1)

	time.Sleep(20 * time.Millisecond)

	c2, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, c2, "job com lease expirado deve ser reivindicável")
	require.Equal(t, c1.ID, c2.ID)
	require.Equal(t, 2, c2.Attempts)
}
