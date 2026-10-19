-- +goose Up
-- Backlog ("quero platinar"): jogos que a pessoa ainda não tem, com preço e alerta de promoção.
CREATE TABLE wishlist (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    platform             text        NOT NULL CHECK (platform IN ('steam', 'psn')),
    store_id             text        NOT NULL, -- appid da Steam / id do produto na PS Store
    title                text        NOT NULL,
    normalized_title     text        NOT NULL,
    cover                text        NOT NULL DEFAULT '',
    target_price_cents   integer     CHECK (target_price_cents IS NULL OR target_price_cents >= 0),
    -- Último preço avisado por push (só avisa de novo se baixar mais).
    notified_price_cents integer,
    added_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, platform, store_id)
);
CREATE INDEX idx_wishlist_item ON wishlist (platform, store_id);

-- Cache de preço por item de loja (compartilhado: o preço não é de ninguém). BRL, centavos.
CREATE TABLE game_prices (
    platform      text        NOT NULL,
    store_id      text        NOT NULL,
    status        text        NOT NULL CHECK (status IN ('ok', 'not_found', 'error')),
    currency      text        NOT NULL DEFAULT 'BRL',
    price_cents   integer,
    regular_cents integer,
    cut           integer     NOT NULL DEFAULT 0,
    low_cents     integer,    -- menor preço histórico (ITAD; só Steam/PC)
    shop          text        NOT NULL DEFAULT '',
    url           text        NOT NULL DEFAULT '',
    fetched_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (platform, store_id)
);

-- +goose Down
DROP TABLE game_prices;
DROP TABLE wishlist;
