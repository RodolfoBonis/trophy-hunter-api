-- +goose Up
-- hero = arte horizontal do jogo (Steam: header da loja). Usada no card da biblioteca (16:10) e no
-- cabeçalho da página do jogo; cover segue sendo a capa vertical. Vazio → o front cai na cover.
ALTER TABLE games ADD COLUMN hero text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE games DROP COLUMN hero;
