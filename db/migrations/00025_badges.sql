-- +goose Up
-- Medalhas do app (catálogo em internal/badges). season = '' nas medalhas fixas; 'YYYY-MM' nas de
-- temporada. Ganha uma vez e fica (mesmo que um jogo seja desvinculado depois).
CREATE TABLE user_badges (
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    badge_key  text        NOT NULL,
    season     text        NOT NULL DEFAULT '',
    earned_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, badge_key, season)
);
CREATE INDEX idx_user_badges_user ON user_badges (user_id, earned_at DESC);

-- +goose Down
DROP TABLE user_badges;
