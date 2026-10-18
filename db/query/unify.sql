-- name: AutoLinkUnified :execrows
-- Liga automaticamente o mesmo jogo em plataformas diferentes (ignora o que o usuário travou).
-- Idempotente. Critério: mesmo título normalizado E número de troféus compatível com alguma versão
-- da outra plataforma (diferença ≤ 5 ou ≤ 25%; 0 = sem conquistas, compatível). O nome sozinho
-- confunde remakes/reboots homônimos (Lords of the Fallen 2014 × 2023, RE4 2005 × 2023) — esses
-- ficam para o vínculo manual. A decisão é por versão (platform + external_id), não por título.
WITH keys AS (
    SELECT g.user_id, g.normalized_title, g.platform, g.external_id, MIN(g.title) AS title, MAX(gp.total) AS total
    FROM games g
    JOIN game_progress gp ON gp.id = g.id
    -- Só versões já sincronizadas (troféus baixados) ou sabidamente sem conquistas: antes disso o
    -- total é 0 e passaria como compatível — elas entram num AutoLink seguinte.
    WHERE NOT g.unify_locked AND g.normalized_title <> ''
      AND (g.schema_synced_at IS NOT NULL OR NOT g.has_achievements)
    GROUP BY g.user_id, g.normalized_title, g.platform, g.external_id
),
linked AS (
    -- Vínculo só entre jogos da MESMA pessoa.
    SELECT DISTINCT a.user_id, a.normalized_title, a.platform, a.external_id, a.title
    FROM keys a
    JOIN keys b ON b.user_id = a.user_id AND b.normalized_title = a.normalized_title AND b.platform <> a.platform
    WHERE a.total = 0 OR b.total = 0
       OR abs(a.total - b.total) <= 5
       OR abs(a.total - b.total)::float8 / greatest(a.total, b.total) <= 0.25
),
titles AS (
    SELECT normalized_title, MIN(title) AS title FROM linked GROUP BY normalized_title
),
ins AS (
    INSERT INTO unified_games (title, normalized_title)
    SELECT title, normalized_title FROM titles
    ON CONFLICT (normalized_title) WHERE NOT manual DO UPDATE SET updated_at = now()
    RETURNING id, normalized_title
)
UPDATE games g SET unified_game_id = ins.id, updated_at = now()
FROM ins
JOIN linked l ON l.normalized_title = ins.normalized_title
WHERE g.user_id = l.user_id AND g.platform = l.platform AND g.external_id = l.external_id AND NOT g.unify_locked
  AND g.unified_game_id IS DISTINCT FROM ins.id;

-- name: CreateManualUnified :one
INSERT INTO unified_games (title, normalized_title, manual) VALUES ($1, $2, true) RETURNING *;

-- name: SetUnifiedForGameKey :exec
-- Aplica (ou remove, com NULL) o vínculo a TODAS as contas que têm o jogo (platform+external_id) e
-- trava a decisão do usuário.
UPDATE games SET unified_game_id = sqlc.narg(unified_game_id)::uuid, unify_locked = true, updated_at = now()
WHERE user_id = sqlc.arg(user_id)::uuid AND platform = sqlc.arg(platform) AND external_id = sqlc.arg(external_id);

-- name: CrossPlatformSiblings :many
-- O mesmo jogo nas OUTRAS plataformas (conta mais avançada de cada uma), para o lado a lado.
SELECT b.id, b.platform, b.external_id, b.title, b.cover, b.hero, b.total, b.earned, b.completion_pct, b.last_unlock_at
FROM best_games b
JOIN games g ON g.id = b.id
WHERE b.user_id = sqlc.arg(user_id)::uuid AND g.unified_game_id = sqlc.arg(unified_game_id) AND b.platform <> sqlc.arg(platform)
ORDER BY b.platform, b.completion_pct DESC;

-- name: LinkCandidates :many
-- Jogos da OUTRA plataforma para vincular manualmente (busca por título).
SELECT b.id, b.platform, b.external_id, b.title, b.cover, b.hero, b.total, b.earned, b.completion_pct, b.last_unlock_at
FROM best_games b
JOIN games g ON g.id = b.id
WHERE b.user_id = sqlc.arg(user_id)::uuid AND b.platform <> sqlc.arg(platform) AND g.normalized_title ILIKE '%' || sqlc.arg(q)::text || '%' ESCAPE '\'
ORDER BY similarity(g.normalized_title, sqlc.arg(q)::text) DESC, b.title
LIMIT 10;
