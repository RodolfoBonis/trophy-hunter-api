-- +goose Up
-- Unificação cross-platform (mesmo jogo na Steam e na PSN):
--   - automática: títulos normalizados IGUAIS em plataformas diferentes viram um unified_games;
--   - manual: o usuário vincula/desvincula; unify_locked = a decisão do usuário vence e a
--     automática não mexe mais naquele jogo.
-- Similaridade aproximada NÃO liga sozinha (ex.: "Black Ops II" × "Black Ops III" = 0.96).
ALTER TABLE games ADD COLUMN unify_locked boolean NOT NULL DEFAULT false;
CREATE INDEX idx_games_unified_game ON games (unified_game_id) WHERE unified_game_id IS NOT NULL;
CREATE UNIQUE INDEX uq_unified_games_auto_title ON unified_games (normalized_title) WHERE NOT manual;

-- +goose Down
DROP INDEX uq_unified_games_auto_title;
DROP INDEX idx_games_unified_game;
ALTER TABLE games DROP COLUMN unify_locked;
