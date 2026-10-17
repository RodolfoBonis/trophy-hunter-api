package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresQueue implementa Queue sobre a tabela jobs.
type PostgresQueue struct {
	pool    *pgxpool.Pool
	backoff BackoffConfig
}

// NewPostgres constrói a fila sobre o pool dado.
func NewPostgres(pool *pgxpool.Pool, backoff BackoffConfig) *PostgresQueue {
	if backoff.Base <= 0 {
		backoff = DefaultBackoff
	}
	return &PostgresQueue{pool: pool, backoff: backoff}
}

const defaultMaxAttempts = 5

// Enqueue insere um job, respeitando o dedupe via ON CONFLICT no índice parcial único.
func (q *PostgresQueue) Enqueue(ctx context.Context, p EnqueueParams) (*Job, bool, error) {
	payload := []byte("{}")
	if p.Payload != nil {
		b, err := json.Marshal(p.Payload)
		if err != nil {
			return nil, false, fmt.Errorf("marshal payload: %w", err)
		}
		payload = b
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	runAt := p.RunAt
	if runAt.IsZero() {
		runAt = time.Now()
	}
	var dedupe *string
	if p.DedupeKey != "" {
		dedupe = &p.DedupeKey
	}

	const stmt = `
INSERT INTO jobs (kind, payload, status, run_at, max_attempts, dedupe_key)
VALUES ($1, $2, 'queued', $3, $4, $5)
ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('queued','running')
DO NOTHING
RETURNING id, kind, payload, status, run_at, attempts, max_attempts, last_error`

	row := q.pool.QueryRow(ctx, stmt, p.Kind, payload, runAt, maxAttempts, dedupe)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// Conflito de dedupe: já há um job ativo idêntico.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("enqueue job: %w", err)
	}
	return job, true, nil
}

// Claim reivindica o próximo job elegível. Pega jobs queued prontos OU running com lease expirado
// (recuperação após crash). Incrementa attempts no claim.
func (q *PostgresQueue) Claim(ctx context.Context, lease time.Duration) (*Job, error) {
	const stmt = `
UPDATE jobs SET
    status = 'running',
    locked_until = now() + $1::interval,
    attempts = attempts + 1,
    updated_at = now()
WHERE id = (
    SELECT id FROM jobs
    WHERE run_at <= now()
      AND (status = 'queued' OR (status = 'running' AND locked_until < now()))
    ORDER BY run_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING id, kind, payload, status, run_at, attempts, max_attempts, last_error`

	row := q.pool.QueryRow(ctx, stmt, lease)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	return job, nil
}

// Complete marca o job como concluído.
func (q *PostgresQueue) Complete(ctx context.Context, id string) error {
	_, err := q.pool.Exec(ctx,
		`UPDATE jobs SET status='done', locked_until=NULL, last_error='', updated_at=now() WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	return nil
}

// Fail reagenda o job com backoff enquanto houver tentativas; esgotadas, marca 'dead'.
func (q *PostgresQueue) Fail(ctx context.Context, id string, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	const stmt = `
UPDATE jobs SET
    status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'queued' END,
    run_at = CASE WHEN attempts >= max_attempts THEN run_at ELSE now() + $2::interval END,
    locked_until = NULL,
    last_error = $3,
    updated_at = now()
WHERE id = $1`
	// O atraso é calculado a partir das attempts já incrementadas no claim.
	delay := q.nextDelay(ctx, id)
	if _, err := q.pool.Exec(ctx, stmt, id, delay, truncate(msg, 2000)); err != nil {
		return fmt.Errorf("fail job: %w", err)
	}
	return nil
}

// nextDelay lê as attempts atuais do job e calcula o backoff exponencial com jitter.
func (q *PostgresQueue) nextDelay(ctx context.Context, id string) time.Duration {
	var attempts int
	if err := q.pool.QueryRow(ctx, `SELECT attempts FROM jobs WHERE id=$1`, id).Scan(&attempts); err != nil {
		attempts = 1
	}
	return q.backoffFor(attempts)
}

// backoffFor devolve o atraso para a n-ésima tentativa (n>=1): Base*2^(n-1) com teto e jitter.
func (q *PostgresQueue) backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exp := math.Pow(2, float64(attempt-1))
	delay := float64(q.backoff.Base) * exp
	if maxDelay := float64(q.backoff.Max); delay > maxDelay {
		delay = maxDelay
	}
	if q.backoff.Jitter > 0 {
		// jitter simétrico: delay * (1 ± jitter)
		factor := 1 + q.backoff.Jitter*(2*rand.Float64()-1) //nolint:gosec // jitter não-criptográfico
		delay *= factor
	}
	return time.Duration(delay)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (*Job, error) {
	var j Job
	var payload []byte
	var runAt time.Time
	if err := row.Scan(&j.ID, &j.Kind, &payload, &j.Status, &runAt, &j.Attempts, &j.MaxAttempts, &j.LastError); err != nil {
		return nil, err
	}
	j.Payload = payload
	j.RunAt = runAt
	return &j, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
