-- name: GetIntegration :one
SELECT * FROM integrations WHERE platform = $1;

-- name: ListIntegrations :many
SELECT * FROM integrations ORDER BY platform;

-- name: UpsertIntegrationCredentials :one
-- Cria/atualiza a conexão com uma plataforma gravando as credenciais cifradas e a identidade da
-- conta. Reseta initial_sync_done=false para que o próximo full sync (re)conecte sem gerar eventos.
INSERT INTO integrations (
    platform, status, account_id, display_name, avatar, credentials_enc, key_version,
    last_error, initial_sync_done, updated_at
) VALUES (
    $1, 'connected', $2, $3, $4, $5, $6, '', false, now()
)
ON CONFLICT (platform) DO UPDATE SET
    status            = 'connected',
    account_id        = EXCLUDED.account_id,
    display_name      = EXCLUDED.display_name,
    avatar            = EXCLUDED.avatar,
    credentials_enc   = EXCLUDED.credentials_enc,
    key_version       = EXCLUDED.key_version,
    last_error        = '',
    initial_sync_done = false,
    updated_at        = now()
RETURNING *;

-- name: UpdateIntegrationStatus :exec
UPDATE integrations
SET status = $2, last_error = $3, updated_at = now()
WHERE platform = $1;

-- name: UpdateIntegrationAccount :exec
UPDATE integrations
SET display_name = $2, avatar = $3, account_id = $4, updated_at = now()
WHERE platform = $1;

-- name: MarkIntegrationSynced :exec
UPDATE integrations
SET last_synced_at = now(), status = 'connected', last_error = '',
    initial_sync_done = initial_sync_done OR sqlc.arg(mark_initial)::boolean,
    updated_at = now()
WHERE platform = $1;

-- name: DeleteIntegration :exec
DELETE FROM integrations WHERE platform = $1;
