-- +goose Up
-- integrations guarda a conexão com cada plataforma (single-user: no máx. uma por plataforma).
-- credentials_enc é o blob AES-256-GCM (ver internal/platform/crypto); NUNCA retornado pela API.
-- initial_sync_done marca que o primeiro full sync terminou: o diff só gera unlock_events depois
-- disso (o sync inicial não cria eventos — ver internal/sync).
CREATE TABLE integrations (
    id                       uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    platform                 text        NOT NULL UNIQUE
        CHECK (platform IN ('steam', 'psn')),
    status                   text        NOT NULL DEFAULT 'disabled'
        CHECK (status IN ('connected', 'needs_reconnect', 'error_private', 'error', 'disabled')),
    account_id               text        NOT NULL DEFAULT '',
    display_name             text        NOT NULL DEFAULT '',
    avatar                   text        NOT NULL DEFAULT '',
    credentials_enc          bytea,
    key_version              integer     NOT NULL DEFAULT 0,
    access_token_expires_at  timestamptz,
    refresh_token_expires_at timestamptz,
    last_error               text        NOT NULL DEFAULT '',
    last_synced_at           timestamptz,
    initial_sync_done        boolean     NOT NULL DEFAULT false,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE integrations;
