-- +goose Up
-- unlock_events registra a transição unearned→earned detectada APÓS o sync inicial (fonte dos
-- toasts, cerimônia de platina e linha do tempo). seen_at/pushed_at controlam UI e push.
CREATE TABLE unlock_events (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    trophy_id   uuid        NOT NULL REFERENCES trophies (id) ON DELETE CASCADE,
    detected_at timestamptz NOT NULL DEFAULT now(),
    seen_at     timestamptz,
    pushed_at   timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_unlock_events_trophy   ON unlock_events (trophy_id);
CREATE INDEX idx_unlock_events_detected ON unlock_events (detected_at DESC);
CREATE INDEX idx_unlock_events_unseen   ON unlock_events (seen_at) WHERE seen_at IS NULL;

-- +goose Down
DROP TABLE unlock_events;
