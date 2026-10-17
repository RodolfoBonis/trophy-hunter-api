-- name: AutoLinkUnified :execrows
-- Liga automaticamente jogos com o MESMO título normalizado em plataformas diferentes (ignora os
-- que o usuário travou). Idempotente.
WITH titles AS (
    SELECT normalized_title, MIN(title) AS title
    FROM games
    WHERE NOT unify_locked AND normalized_title <> ''
    GROUP BY normalized_title
    HAVING COUNT(DISTINCT platform) > 1
),
ins AS (
    INSERT INTO unified_games (title, normalized_title)
    SELECT title, normalized_title FROM titles
    ON CONFLICT (normalized_title) WHERE NOT manual DO UPDATE SET updated_at = now()
    RETURNING id, normalized_title
)
UPDATE games g SET unified_game_id = ins.id, updated_at = now()
FROM ins
WHERE g.normalized_title = ins.normalized_title AND NOT g.unify_locked
  AND g.unified_game_id IS DISTINCT FROM ins.id;

-- name: CreateManualUnified :one
INSERT INTO unified_games (title, normalized_title, manual) VALUES ($1, $2, true) RETURNING *;

-- name: SetUnifiedForGameKey :exec
-- Aplica (ou remove, com NULL) o vínculo a TODAS as contas que têm o jogo (platform+external_id) e
-- trava a decisão do usuário.
UPDATE games SET unified_game_id = sqlc.narg(unified_game_id)::uuid, unify_locked = true, updated_at = now()
WHERE platform = sqlc.arg(platform) AND external_id = sqlc.arg(external_id);

-- name: CrossPlatformSiblings :many
-- O mesmo jogo nas OUTRAS plataformas (conta mais avançada de cada uma), para o lado a lado.
SELECT b.id, b.platform, b.external_id, b.title, b.cover, b.hero, b.total, b.earned, b.completion_pct, b.last_unlock_at
FROM best_games b
JOIN games g ON g.id = b.id
WHERE g.unified_game_id = sqlc.arg(unified_game_id) AND b.platform <> sqlc.arg(platform)
ORDER BY b.platform, b.completion_pct DESC;

-- name: LinkCandidates :many
-- Jogos da OUTRA plataforma para vincular manualmente (busca por título).
SELECT b.id, b.platform, b.external_id, b.title, b.cover, b.hero, b.total, b.earned, b.completion_pct, b.last_unlock_at
FROM best_games b
JOIN games g ON g.id = b.id
WHERE b.platform <> sqlc.arg(platform) AND g.normalized_title ILIKE '%' || sqlc.arg(q)::text || '%' ESCAPE '\'
ORDER BY similarity(g.normalized_title, sqlc.arg(q)::text) DESC, b.title
LIMIT 10;
