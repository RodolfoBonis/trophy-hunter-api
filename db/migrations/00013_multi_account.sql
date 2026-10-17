-- +goose Up
-- Várias contas por plataforma (ex.: duas contas Steam). A integração passa a ser identificada pelo
-- id (não mais pela plataforma) e cada jogo pertence a UMA conta: conquistas são por conta, então o
-- mesmo appid em duas contas vira duas linhas em games (agrupadas por platform+external_id na
-- biblioteca, que mostra a conta mais avançada).
--
-- Backfill: a(s) integração(ões) existente(s) ganham label = display_name (ou 'Principal') e todos os
-- jogos existentes são vinculados à integração da sua plataforma (antes havia no máx. uma).

ALTER TABLE integrations DROP CONSTRAINT integrations_platform_key;
ALTER TABLE integrations ADD COLUMN label text NOT NULL DEFAULT '';
UPDATE integrations SET label = COALESCE(NULLIF(display_name, ''), 'Principal');
-- Mesma conta não pode ser conectada duas vezes (account_id vazio = ainda não resolvido).
CREATE UNIQUE INDEX uq_integrations_account ON integrations (platform, account_id) WHERE account_id <> '';
CREATE INDEX idx_integrations_platform ON integrations (platform);

ALTER TABLE games ADD COLUMN integration_id uuid REFERENCES integrations (id) ON DELETE CASCADE;
UPDATE games g SET integration_id = i.id FROM integrations i WHERE i.platform = g.platform;
-- Jogos órfãos (integração removida antes desta migration, quando não havia FK) não têm dono.
DELETE FROM games WHERE integration_id IS NULL;
ALTER TABLE games ALTER COLUMN integration_id SET NOT NULL;

ALTER TABLE games DROP CONSTRAINT games_platform_external_id_key;
ALTER TABLE games ADD CONSTRAINT games_integration_external_key UNIQUE (integration_id, external_id);
-- Agrupamento da biblioteca (mesmo jogo em várias contas).
CREATE INDEX idx_games_platform_external ON games (platform, external_id);

-- +goose Down
DROP INDEX idx_games_platform_external;
ALTER TABLE games DROP CONSTRAINT games_integration_external_key;
-- Volta ao modelo de uma conta por plataforma: mantém só os jogos da conta mais antiga.
DELETE FROM games g USING integrations i
WHERE i.id = g.integration_id
  AND i.id <> (SELECT i2.id FROM integrations i2 WHERE i2.platform = i.platform ORDER BY i2.created_at LIMIT 1);
ALTER TABLE games ADD CONSTRAINT games_platform_external_id_key UNIQUE (platform, external_id);
ALTER TABLE games DROP COLUMN integration_id;
DELETE FROM integrations i
WHERE i.id <> (SELECT i2.id FROM integrations i2 WHERE i2.platform = i.platform ORDER BY i2.created_at LIMIT 1);
DROP INDEX idx_integrations_platform;
DROP INDEX uq_integrations_account;
ALTER TABLE integrations DROP COLUMN label;
ALTER TABLE integrations ADD CONSTRAINT integrations_platform_key UNIQUE (platform);
