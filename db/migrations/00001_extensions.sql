-- +goose Up
-- pg_trgm habilita busca fuzzy por título (biblioteca e unificação cross-platform na fase 3).
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- +goose Down
DROP EXTENSION IF EXISTS pg_trgm;
