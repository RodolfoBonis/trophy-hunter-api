-- Integrações: várias contas por plataforma (ver migration 00013). Tudo é endereçado pelo id.

-- name: GetIntegration :one
SELECT * FROM integrations WHERE id = $1;

-- name: GetIntegrationByAccount :one
SELECT * FROM integrations WHERE platform = $1 AND account_id = $2;

-- name: ListIntegrations :many
SELECT * FROM integrations ORDER BY platform, created_at;

-- name: ListIntegrationsByPlatform :many
SELECT * FROM integrations WHERE platform = $1 ORDER BY created_at;

-- name: CreateIntegration :one
-- Cria a conexão com uma conta gravando as credenciais cifradas e a identidade da conta.
-- initial_sync_done=false: o primeiro full sync não gera unlock_events.
INSERT INTO integrations (
    platform, label, status, account_id, display_name, avatar, credentials_enc, key_version,
    last_error, initial_sync_done, updated_at
) VALUES (
    $1, $2, 'connected', $3, $4, $5, $6, $7, '', false, now()
)
RETURNING *;

-- name: UpdateIntegrationCredentials :exec
-- Troca as credenciais cifradas (ex.: nova API key) sem mexer na identidade nem no histórico.
UPDATE integrations
SET credentials_enc = $2, key_version = $3, updated_at = now()
WHERE id = $1;

-- name: UpdateIntegrationLabel :exec
UPDATE integrations SET label = $2, updated_at = now() WHERE id = $1;

-- name: UpdateIntegrationStatus :exec
UPDATE integrations
SET status = $2, last_error = $3, updated_at = now()
WHERE id = $1;

-- name: MarkIntegrationSynced :exec
-- Fecho de sync BEM-SUCEDIDO: marca last_synced_at, volta a status connected e limpa o erro.
-- initial_sync_done só é ligado quando mark_initial (full inicial sem falhas).
UPDATE integrations
SET last_synced_at = now(), status = 'connected', last_error = '',
    initial_sync_done = initial_sync_done OR sqlc.arg(mark_initial)::boolean,
    updated_at = now()
WHERE id = $1;

-- name: TouchIntegrationSynced :exec
-- Fecho de sync COM falhas: só atualiza last_synced_at (preserva o status de erro que os jobs
-- filhos mortos já definiram). Não volta a connected.
UPDATE integrations
SET last_synced_at = now(), updated_at = now()
WHERE id = $1;

-- name: DeleteIntegration :exec
-- Remove a conta; os jogos dela (e troféus/grupos/eventos) caem em cascata.
DELETE FROM integrations WHERE id = $1;

-- name: UpdateIntegrationTokens :exec
-- Grava tokens renovados (PSN): credenciais cifradas + validades (para renovar antes de expirar e
-- avisar quando o refresh estiver perto de vencer).
UPDATE integrations
SET credentials_enc = $2, key_version = $3,
    access_token_expires_at = $4, refresh_token_expires_at = $5, updated_at = now()
WHERE id = $1;

-- name: UpdateIntegrationProfile :exec
UPDATE integrations SET display_name = $2, avatar = $3, updated_at = now() WHERE id = $1;
