-- name: GetGameByID :one
SELECT * FROM games WHERE id = $1;

-- name: GetGameByExternal :one
SELECT * FROM games WHERE platform = $1 AND external_id = $2;

-- name: ListGamesByPlatform :many
SELECT * FROM games WHERE platform = $1 ORDER BY id;

-- name: UpsertGame :one
-- Insere/atualiza a definição e os dados remotos do jogo, preservando campos do usuário
-- (abandoned, priority, notes, tags, unified_game_id).
INSERT INTO games (
    platform, external_id, np_service_name, title, normalized_title, icon, cover, platforms,
    has_achievements, last_played_at, playtime_minutes, remote_updated_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now()
)
ON CONFLICT (platform, external_id) DO UPDATE SET
    np_service_name   = EXCLUDED.np_service_name,
    title             = EXCLUDED.title,
    normalized_title  = EXCLUDED.normalized_title,
    icon              = EXCLUDED.icon,
    cover             = EXCLUDED.cover,
    platforms         = EXCLUDED.platforms,
    has_achievements  = EXCLUDED.has_achievements,
    last_played_at    = EXCLUDED.last_played_at,
    playtime_minutes  = EXCLUDED.playtime_minutes,
    remote_updated_at = EXCLUDED.remote_updated_at,
    updated_at        = now()
RETURNING *;

-- name: SetGameSchemaSynced :exec
UPDATE games SET schema_synced_at = now(), has_platinum = $2, updated_at = now() WHERE id = $1;

-- name: SetGamePercentagesSynced :exec
UPDATE games SET percentages_synced_at = now(), updated_at = now() WHERE id = $1;

-- name: ListGames :many
WITH agg AS (
    SELECT g.*,
        COUNT(t.id)                                                        AS total,
        COUNT(t.id) FILTER (WHERE t.earned)                                AS earned,
        COALESCE(AVG(t.rarity_pct) FILTER (WHERE NOT t.earned), 0)::float8  AS avg_remaining_rarity
    FROM games g
    LEFT JOIN trophies t ON t.game_id = g.id
    GROUP BY g.id
),
scored AS (
    SELECT *,
        (total - earned)::bigint                                             AS remaining,
        (CASE WHEN total > 0 THEN earned::float8 / total ELSE 0 END)::float8 AS completion_pct,
        CASE
            WHEN abandoned                                      THEN 'abandoned'
            WHEN total > 0 AND earned = total                   THEN 'completed'
            WHEN total > 0 AND earned::float8 / total >= sqlc.arg(almost)::float8 THEN 'almost'
            WHEN earned > 0                                     THEN 'in_progress'
            ELSE 'not_started'
        END                                                                  AS status_bucket
    FROM agg
)
SELECT * FROM scored
WHERE (sqlc.narg(platform)::text IS NULL OR platform = sqlc.narg(platform)::text)
  AND (sqlc.narg(has_achievements)::boolean IS NULL OR has_achievements = sqlc.narg(has_achievements)::boolean)
  AND (sqlc.narg(q)::text IS NULL OR normalized_title ILIKE '%' || sqlc.narg(q)::text || '%')
  AND (sqlc.narg(status)::text IS NULL OR status_bucket = sqlc.narg(status)::text)
ORDER BY
    (CASE WHEN sqlc.arg(sort)::text = 'title'      THEN normalized_title     END) ASC  NULLS LAST,
    (CASE WHEN sqlc.arg(sort)::text = 'completion' THEN completion_pct       END) DESC NULLS LAST,
    (CASE WHEN sqlc.arg(sort)::text = 'remaining'  THEN remaining            END) ASC  NULLS LAST,
    (CASE WHEN sqlc.arg(sort)::text = 'rarity'     THEN avg_remaining_rarity END) DESC NULLS LAST,
    (CASE WHEN sqlc.arg(sort)::text NOT IN ('title','completion','remaining','rarity')
          THEN last_played_at END) DESC NULLS LAST,
    id
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: CountGames :one
WITH agg AS (
    SELECT g.id, g.platform, g.has_achievements, g.normalized_title, g.abandoned,
        COUNT(t.id)                         AS total,
        COUNT(t.id) FILTER (WHERE t.earned) AS earned
    FROM games g
    LEFT JOIN trophies t ON t.game_id = g.id
    GROUP BY g.id
),
scored AS (
    SELECT *,
        CASE
            WHEN abandoned                                      THEN 'abandoned'
            WHEN total > 0 AND earned = total                   THEN 'completed'
            WHEN total > 0 AND earned::float8 / total >= sqlc.arg(almost)::float8 THEN 'almost'
            WHEN earned > 0                                     THEN 'in_progress'
            ELSE 'not_started'
        END AS status_bucket
    FROM agg
)
SELECT COUNT(*) FROM scored
WHERE (sqlc.narg(platform)::text IS NULL OR platform = sqlc.narg(platform)::text)
  AND (sqlc.narg(has_achievements)::boolean IS NULL OR has_achievements = sqlc.narg(has_achievements)::boolean)
  AND (sqlc.narg(q)::text IS NULL OR normalized_title ILIKE '%' || sqlc.narg(q)::text || '%')
  AND (sqlc.narg(status)::text IS NULL OR status_bucket = sqlc.narg(status)::text);
