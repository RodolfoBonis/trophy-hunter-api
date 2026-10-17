-- +goose Up
-- unified_games agrupa o "mesmo jogo" entre plataformas (ex.: versão Steam + PS5). Sem uso de
-- escrita na fase 1 (só existe o esquesto); a unificação automática/manual chega na fase 3.
CREATE TABLE unified_games (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    title            text        NOT NULL,
    normalized_title text        NOT NULL,
    cover            text,
    manual           boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_unified_games_normalized_trgm
    ON unified_games USING gin (normalized_title gin_trgm_ops);

-- +goose Down
DROP TABLE unified_games;
