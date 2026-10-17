-- +goose Up
-- jobs é a fila em Postgres (ver internal/platform/queue): claim com FOR UPDATE SKIP LOCKED +
-- lease (locked_until), backoff exponencial com jitter e dedupe.
--   - O índice parcial único em dedupe_key impede enfileirar o mesmo trabalho enquanto um idêntico
--     está queued/running (NULL não participa de unique → jobs sem dedupe_key convivem).
--   - idx_jobs_claim acelera o claim (status, run_at) do próximo job elegível.
CREATE TABLE jobs (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL,
    payload      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    status       text        NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'done', 'failed', 'dead')),
    run_at       timestamptz NOT NULL DEFAULT now(),
    attempts     integer     NOT NULL DEFAULT 0,
    max_attempts integer     NOT NULL DEFAULT 5,
    locked_until timestamptz,
    last_error   text        NOT NULL DEFAULT '',
    dedupe_key   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_jobs_dedupe_active
    ON jobs (dedupe_key)
    WHERE dedupe_key IS NOT NULL AND status IN ('queued', 'running');

CREATE INDEX idx_jobs_claim ON jobs (status, run_at) WHERE status = 'queued';

-- +goose Down
DROP TABLE jobs;
