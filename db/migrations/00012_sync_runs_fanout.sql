-- +goose Up
-- Fan-out do sync: um sync_platform lista a biblioteca e emite um sync_game por jogo; o sync_run
-- é o run PAI que agrega o progresso dos filhos.
--   - games_total: nº de jobs filhos realmente enfileirados (ajustado por dedupe).
--   - games_done/games_failed: filhos que terminaram (sucesso / falha terminal).
--   - items_changed já existia (agregado de troféus alterados).
--   - fanout_sealed: o pai terminou de enfileirar os filhos e fixou games_total. O run só fecha
--     quando fanout_sealed AND games_done >= games_total (barreira — evita corrida com filhos
--     rápidos que terminam antes do pai selar).
-- games_touched vira legado (não mais populado); mantido para não quebrar linhas antigas.
ALTER TABLE sync_runs
    ADD COLUMN games_total   integer NOT NULL DEFAULT 0,
    ADD COLUMN games_done    integer NOT NULL DEFAULT 0,
    ADD COLUMN games_failed  integer NOT NULL DEFAULT 0,
    ADD COLUMN fanout_sealed boolean NOT NULL DEFAULT false;

-- unlock_events: no máximo UM evento por troféu (o app registra a 1ª conquista; re-detecções do
-- diff não duplicam). InsertUnlockEvent usa ON CONFLICT DO NOTHING contra este índice.
CREATE UNIQUE INDEX uq_unlock_events_trophy ON unlock_events (trophy_id);

-- +goose Down
DROP INDEX IF EXISTS uq_unlock_events_trophy;
ALTER TABLE sync_runs
    DROP COLUMN games_total,
    DROP COLUMN games_done,
    DROP COLUMN games_failed,
    DROP COLUMN fanout_sealed;
