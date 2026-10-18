-- +goose Up
-- Web push (Fase 5): inscrições do navegador (uma por dispositivo/navegador). failures conta envios
-- recusados; 404/410 do push service apagam a inscrição na hora.
CREATE TABLE push_subscriptions (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint        text        NOT NULL UNIQUE,
    p256dh          text        NOT NULL,
    auth            text        NOT NULL,
    user_agent      text        NOT NULL DEFAULT '',
    failures        integer     NOT NULL DEFAULT 0,
    last_success_at timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Anti-spam dos alertas de conta: último status já notificado e quando (ver ClaimStatusNotification).
ALTER TABLE integrations
    ADD COLUMN notified_status text NOT NULL DEFAULT 'connected',
    ADD COLUMN notified_at     timestamptz;

-- +goose Down
ALTER TABLE integrations DROP COLUMN notified_at, DROP COLUMN notified_status;
DROP TABLE push_subscriptions;
