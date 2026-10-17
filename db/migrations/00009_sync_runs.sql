-- +goose Up
-- sync_runs é o log de sincronização exibido na UI (um por execução de sync).
CREATE TABLE sync_runs (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    integration_id uuid        REFERENCES integrations (id) ON DELETE SET NULL,
    platform       text        NOT NULL CHECK (platform IN ('steam', 'psn')),
    scope          text        NOT NULL CHECK (scope IN ('full', 'incremental', 'game')),
    trigger        text        NOT NULL CHECK (trigger IN ('schedule', 'manual')),
    status         text        NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'ok', 'error')),
    started_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz,
    items_changed  integer     NOT NULL DEFAULT 0,
    games_touched  integer     NOT NULL DEFAULT 0,
    error          text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_sync_runs_platform ON sync_runs (platform, started_at DESC);

-- +goose Down
DROP TABLE sync_runs;
