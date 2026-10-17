-- +goose Up
-- settings é um kv jsonb de preferências (intervalos, som, spoiler, limiar "quase lá"…).
CREATE TABLE settings (
    key        text        PRIMARY KEY,
    value      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE settings;
