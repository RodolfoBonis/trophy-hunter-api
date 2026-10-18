-- Usuários (identidade do IdP + papel/estado no app) e convites.

-- name: GetUserByIdentity :one
SELECT * FROM users WHERE issuer = $1 AND subject = $2;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetOwner :one
SELECT * FROM users WHERE role = 'owner';

-- name: ClaimOwner :one
-- O dono entra com o e-mail verificado = OWNER_EMAIL: amarra a identidade atual (iss+sub) à linha
-- do dono — também quando o IdP muda (realm platform → trophy-hunter), sem perder os dados.
UPDATE users SET issuer = $2, subject = $3, email = $4, name = $5, last_seen_at = now(), updated_at = now()
WHERE id = $1 AND role = 'owner'
RETURNING *;

-- name: CreateUser :one
-- Primeiro login de alguém: nasce "pending" (precisa aceitar um convite).
INSERT INTO users (issuer, subject, email, name) VALUES ($1, $2, $3, $4)
ON CONFLICT (issuer, subject) WHERE subject IS NOT NULL DO UPDATE SET updated_at = now()
RETURNING *;

-- name: TouchUser :exec
UPDATE users SET email = $2, name = $3, last_seen_at = now(), updated_at = now() WHERE id = $1;

-- name: ActivateUser :one
-- pending (1º convite) ou removed (readmissão com um convite novo; os dados não apagados voltam).
UPDATE users SET status = 'active', invited_by = $2, updated_at = now()
WHERE id = $1 AND status IN ('pending', 'removed')
RETURNING *;

-- name: ListMembers :many
-- Quem já entrou (ativos e removidos), dono primeiro.
SELECT u.*, (SELECT count(*) FROM integrations i WHERE i.user_id = u.id)::int AS accounts
FROM users u
WHERE u.status <> 'pending'
ORDER BY (u.role = 'owner') DESC, u.created_at;

-- name: RemoveMember :execrows
-- Revoga o acesso (não-dono). Os dados ficam (podem ser apagados com DeleteMember).
UPDATE users SET status = 'removed', updated_at = now() WHERE id = $1 AND role <> 'owner';

-- name: DeleteMember :execrows
-- Apaga a pessoa e TODOS os dados dela (contas, jogos, chat, push — ON DELETE CASCADE).
DELETE FROM users WHERE id = $1 AND role <> 'owner';

-- name: CreateInvite :one
INSERT INTO invites (code_hash, created_by, expires_at, max_uses) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: ListInvites :many
-- Convites ainda úteis (não revogados, não vencidos, com usos sobrando).
SELECT * FROM invites
WHERE revoked_at IS NULL AND expires_at > now() AND used_count < max_uses
ORDER BY created_at DESC;

-- name: RevokeInvite :execrows
UPDATE invites SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: ConsumeInvite :one
-- Usa o convite (atômico): só funciona se válido; devolve quem convidou.
UPDATE invites SET used_count = used_count + 1
WHERE code_hash = $1 AND revoked_at IS NULL AND expires_at > now() AND used_count < max_uses
RETURNING created_by;
