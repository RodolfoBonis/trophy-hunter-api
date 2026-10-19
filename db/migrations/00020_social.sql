-- +goose Up
-- Recursos sociais entre membros (perfil, comparar no jogo, feed, ranking). OPT-IN: cada pessoa
-- liga "compartilhar meu progresso"; até ligar, ninguém vê nada dela — e ela também não vê os outros
-- (reciprocidade).
ALTER TABLE users ADD COLUMN share_progress    boolean     NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN share_progress_at timestamptz;

-- Feed e ranking varrem desbloqueios por data.
CREATE INDEX idx_trophies_earned_at ON trophies (earned_at DESC) WHERE earned;
-- "Amigos neste jogo": casa o mesmo jogo por título normalizado (Steam × PSN).
CREATE INDEX idx_games_normalized_title ON games (normalized_title);

-- +goose Down
DROP INDEX idx_games_normalized_title;
DROP INDEX idx_trophies_earned_at;
ALTER TABLE users DROP COLUMN share_progress_at;
ALTER TABLE users DROP COLUMN share_progress;
