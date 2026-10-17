-- +goose Up
-- trophy_groups separa base vs DLC. Steam não tem grupos → um grupo "default" is_base=true por jogo.
-- PSN tem grupos (default + DLCs).
CREATE TABLE trophy_groups (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    game_id     uuid        NOT NULL REFERENCES games (id) ON DELETE CASCADE,
    external_id text        NOT NULL,
    name        text        NOT NULL DEFAULT '',
    is_base     boolean     NOT NULL DEFAULT false,
    icon        text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (game_id, external_id)
);

CREATE INDEX idx_trophy_groups_game ON trophy_groups (game_id);

-- +goose Down
DROP TABLE trophy_groups;
