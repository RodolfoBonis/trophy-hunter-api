-- name: UpsertTrophyGroup :one
INSERT INTO trophy_groups (game_id, external_id, name, is_base, icon, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (game_id, external_id) DO UPDATE SET
    name = EXCLUDED.name, is_base = EXCLUDED.is_base, icon = EXCLUDED.icon, updated_at = now()
RETURNING *;

-- name: ListTrophyGroupsByGame :many
SELECT * FROM trophy_groups WHERE game_id = $1 ORDER BY is_base DESC, name;

-- name: UpsertTrophyDefinition :one
-- Upserta a DEFINIÇÃO do troféu (preserva earned/earned_at e campos do usuário no conflito).
INSERT INTO trophies (
    game_id, group_id, external_id, name, description, type, hidden, icon, icon_locked,
    progress, target, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now()
)
ON CONFLICT (game_id, external_id) DO UPDATE SET
    group_id    = EXCLUDED.group_id,
    name        = EXCLUDED.name,
    description = EXCLUDED.description,
    type        = EXCLUDED.type,
    hidden      = EXCLUDED.hidden,
    icon        = EXCLUDED.icon,
    icon_locked = EXCLUDED.icon_locked,
    progress    = EXCLUDED.progress,
    target      = EXCLUDED.target,
    updated_at  = now()
RETURNING *;

-- name: SetTrophyEarned :exec
UPDATE trophies SET earned = $2, earned_at = $3, updated_at = now() WHERE id = $1;

-- name: SetTrophyRarity :exec
UPDATE trophies SET rarity_pct = $2, updated_at = now() WHERE id = $1;

-- name: ListTrophiesByGame :many
SELECT * FROM trophies WHERE game_id = $1 ORDER BY type, name;

-- name: InsertUnlockEvent :exec
INSERT INTO unlock_events (trophy_id, detected_at) VALUES ($1, now());
