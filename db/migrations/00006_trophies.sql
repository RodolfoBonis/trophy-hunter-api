-- +goose Up
-- trophies é a conquista/troféu. type 'achievement' = Steam; platinum/gold/silver/bronze = PSN.
--   - description NULL em ocultas sem descrição (spoiler guard).
--   - rarity_pct é o percentual global (GetGlobalAchievementPercentagesForApp / PSN rarity).
--   - earned/earned_at vêm do progresso do jogador; o diff compara earned para gerar unlock_events.
--   - tags[] restrito por CHECK à taxonomia perdível/online/etc.
CREATE TABLE trophies (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    game_id              uuid        NOT NULL REFERENCES games (id) ON DELETE CASCADE,
    group_id             uuid        NOT NULL REFERENCES trophy_groups (id) ON DELETE CASCADE,
    external_id          text        NOT NULL,
    name                 text        NOT NULL DEFAULT '',
    description          text,
    type                 text        NOT NULL DEFAULT 'achievement'
        CHECK (type IN ('platinum', 'gold', 'silver', 'bronze', 'achievement')),
    hidden               boolean     NOT NULL DEFAULT false,
    icon                 text        NOT NULL DEFAULT '',
    icon_locked          text        NOT NULL DEFAULT '',
    rarity_pct           double precision,
    earned               boolean     NOT NULL DEFAULT false,
    earned_at            timestamptz,
    progress             integer,
    target               integer,
    priority             integer     NOT NULL DEFAULT 0,
    notes                text        NOT NULL DEFAULT '',
    tags                 text[]      NOT NULL DEFAULT '{}'
        CHECK (tags <@ ARRAY['missable','online','grind','difficulty','collectible','buggy']::text[]),
    server_shutdown_risk boolean     NOT NULL DEFAULT false,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (game_id, external_id)
);

CREATE INDEX idx_trophies_game   ON trophies (game_id);
CREATE INDEX idx_trophies_group  ON trophies (group_id);
CREATE INDEX idx_trophies_earned ON trophies (game_id, earned);

-- +goose Down
DROP TABLE trophies;
