-- +goose Up
-- Modo caçada: etapas do roadmap que a pessoa marcou como feitas. Por jogo da plataforma (não por
-- conta), como o próprio roadmap; o índice é a posição da etapa no roadmap atual.
CREATE TABLE hunt_progress (
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    game_platform    text        NOT NULL,
    game_external_id text        NOT NULL,
    stage_idx        integer     NOT NULL CHECK (stage_idx >= 0 AND stage_idx < 100),
    done_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, game_platform, game_external_id, stage_idx)
);

-- +goose Down
DROP TABLE hunt_progress;
