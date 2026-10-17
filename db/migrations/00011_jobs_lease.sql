-- +goose Up
-- Endurece a fila: lease_token (posse do job) + índice de reclaim.
--   - lease_token é um uuid gerado no claim. Complete/Fail só afetam o job se o token bater
--     (status='running' AND lease_token=$token): um worker com lease expirado que "acorda" não
--     consegue mais marcar done/fail um job que outro já reivindicou (evita duplo processamento).
--   - idx_jobs_reclaim acelera o claim de jobs com lease expirado (status='running', locked_until).
ALTER TABLE jobs ADD COLUMN lease_token uuid;

CREATE INDEX idx_jobs_reclaim ON jobs (locked_until) WHERE status = 'running';

-- +goose Down
DROP INDEX IF EXISTS idx_jobs_reclaim;
ALTER TABLE jobs DROP COLUMN lease_token;
