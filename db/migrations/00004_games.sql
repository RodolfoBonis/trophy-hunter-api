-- +goose Up
-- games é um jogo por plataforma (Steam appid ou PSN npCommunicationId).
--   - has_achievements=false quando o jogo não possui conquistas (ainda assim listado).
--   - playtime_minutes espelha playtime_forever (Steam, em minutos).
--   - remote_updated_at espelha rtime_last_played (Steam) — usado no sync incremental.
--   - schema_synced_at / percentages_synced_at datam o último refetch de schema e de raridade.
--   - tags[] usa a mesma taxonomia dos troféus, restrita por CHECK (contido-em).
CREATE TABLE games (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    platform              text        NOT NULL CHECK (platform IN ('steam', 'psn')),
    external_id           text        NOT NULL,
    np_service_name       text,
    unified_game_id       uuid        REFERENCES unified_games (id) ON DELETE SET NULL,
    title                 text        NOT NULL,
    normalized_title      text        NOT NULL DEFAULT '',
    icon                  text        NOT NULL DEFAULT '',
    cover                 text        NOT NULL DEFAULT '',
    platforms             text[]      NOT NULL DEFAULT '{}',
    has_achievements      boolean     NOT NULL DEFAULT true,
    has_platinum          boolean     NOT NULL DEFAULT false,
    last_played_at        timestamptz,
    playtime_minutes      integer     NOT NULL DEFAULT 0,
    remote_updated_at     timestamptz,
    schema_synced_at      timestamptz,
    percentages_synced_at timestamptz,
    abandoned             boolean     NOT NULL DEFAULT false,
    priority              integer     NOT NULL DEFAULT 0,
    notes                 text        NOT NULL DEFAULT '',
    tags                  text[]      NOT NULL DEFAULT '{}'
        CHECK (tags <@ ARRAY['missable','online','grind','difficulty','collectible','buggy']::text[]),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (platform, external_id)
);

CREATE INDEX idx_games_platform           ON games (platform);
CREATE INDEX idx_games_unified            ON games (unified_game_id);
CREATE INDEX idx_games_last_played        ON games (last_played_at DESC NULLS LAST);
CREATE INDEX idx_games_has_achievements   ON games (has_achievements);
CREATE INDEX idx_games_normalized_trgm    ON games USING gin (normalized_title gin_trgm_ops);

-- +goose Down
DROP TABLE games;
