// Package queue é uma fila de jobs durável em Postgres.
//
// Garantias:
//   - Claim atômico com FOR UPDATE SKIP LOCKED: réplicas concorrentes nunca pegam o mesmo job.
//   - Lease (locked_until): se um worker morre segurando um job, o lease expira e outro reivindica.
//   - Dedupe: Enqueue com DedupeKey não-vazio é no-op se já existe um job idêntico queued/running
//     (índice parcial único uq_jobs_dedupe_active).
//   - Backoff exponencial com jitter até max_attempts; exaurido → estado 'dead'.
//
// A interface Queue isola a implementação para permitir trocar por outro broker depois.
package queue

import (
	"context"
	"encoding/json"
	"time"
)

// Status é o estado de um job.
type Status string

// Estados possíveis de um job.
const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusDead    Status = "dead"
)

// Job é uma unidade de trabalho persistida.
type Job struct {
	ID          string
	Kind        string
	Payload     json.RawMessage
	Status      Status
	RunAt       time.Time
	Attempts    int
	MaxAttempts int
	LastError   string
}

// EnqueueParams descreve um job a enfileirar.
type EnqueueParams struct {
	Kind string
	// Payload é serializado em JSON. Pode ser nil (vira {}).
	Payload any
	// DedupeKey, quando não-vazio, impede enfileirar um job idêntico já queued/running.
	DedupeKey string
	// MaxAttempts default 5 quando <= 0.
	MaxAttempts int
	// RunAt agenda o job para o futuro (zero = agora).
	RunAt time.Time
}

// Queue é a fila de jobs. As implementações devem ser seguras para uso concorrente.
type Queue interface {
	// Enqueue insere um job. Com DedupeKey e um job ativo idêntico existente, devolve
	// (nil, false, nil) — enfileiramento suprimido, sem erro.
	Enqueue(ctx context.Context, p EnqueueParams) (job *Job, enqueued bool, err error)
	// Claim reivindica o próximo job elegível (queued ou com lease expirado), marcando-o running
	// com um novo lease. Devolve (nil, nil) quando não há job elegível.
	Claim(ctx context.Context, lease time.Duration) (*Job, error)
	// Complete marca o job como concluído com sucesso.
	Complete(ctx context.Context, id string) error
	// Fail registra uma falha: reagenda com backoff enquanto houver tentativas; senão 'dead'.
	Fail(ctx context.Context, id string, cause error) error
}

// BackoffConfig parametriza o backoff exponencial com jitter.
type BackoffConfig struct {
	Base   time.Duration // atraso base (ex.: 5s)
	Max    time.Duration // teto do atraso (ex.: 1h)
	Jitter float64       // fração de jitter [0,1] aplicada ao atraso (ex.: 0.2 = ±20%)
}

// DefaultBackoff é o backoff padrão da fila.
var DefaultBackoff = BackoffConfig{Base: 5 * time.Second, Max: time.Hour, Jitter: 0.2}
