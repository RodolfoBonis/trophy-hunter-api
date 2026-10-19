-- Retrospectiva (período) e resumo semanal da IA. Datas no fuso de Brasília; jogos contados pela
-- conta mais avançada (best_games), como o resto das estatísticas.

-- name: RecapTotals :one
SELECT
    COUNT(t.id)::int                                                          AS earned,
    (COUNT(t.id) FILTER (WHERE t.type = 'platinum'))::int                     AS platinum,
    (COUNT(t.id) FILTER (WHERE t.type = 'gold'))::int                         AS gold,
    (COUNT(t.id) FILTER (WHERE t.type = 'silver'))::int                       AS silver,
    (COUNT(t.id) FILTER (WHERE t.type = 'bronze'))::int                       AS bronze,
    (COUNT(t.id) FILTER (WHERE t.type = 'achievement'))::int                  AS achievements,
    (COUNT(t.id) FILTER (WHERE t.rarity_pct < 5))::int                        AS rare,
    COUNT(DISTINCT b.id)::int                                                 AS games,
    MIN(t.earned_at)::timestamptz                                             AS first_at,
    MAX(t.earned_at)::timestamptz                                             AS last_at
FROM best_games b
JOIN trophies t ON t.game_id = b.id AND t.earned
WHERE b.user_id = sqlc.arg(user_id)::uuid
  AND t.earned_at >= sqlc.arg(since)::timestamptz AND t.earned_at < sqlc.arg(until)::timestamptz;

-- name: RecapTopGames :many
SELECT b.id, b.title, b.platform, b.external_id, b.cover, b.hero, b.earned, b.total, COUNT(t.id)::int AS unlocked
FROM best_games b
JOIN trophies t ON t.game_id = b.id AND t.earned
WHERE b.user_id = sqlc.arg(user_id)::uuid
  AND t.earned_at >= sqlc.arg(since)::timestamptz AND t.earned_at < sqlc.arg(until)::timestamptz
GROUP BY b.id, b.title, b.platform, b.external_id, b.cover, b.hero, b.earned, b.total
ORDER BY unlocked DESC, b.title
LIMIT sqlc.arg(lim);

-- name: RecapRarest :many
SELECT t.id, t.name, t.type, t.hidden, t.icon, t.rarity_pct::float8 AS rarity_pct, t.earned_at, b.title AS game_title, b.platform
FROM best_games b
JOIN trophies t ON t.game_id = b.id AND t.earned
WHERE b.user_id = sqlc.arg(user_id)::uuid AND t.rarity_pct IS NOT NULL
  AND t.earned_at >= sqlc.arg(since)::timestamptz AND t.earned_at < sqlc.arg(until)::timestamptz
ORDER BY t.rarity_pct ASC, t.earned_at DESC
LIMIT sqlc.arg(lim);

-- name: RecapCompletions :many
SELECT b.id, b.title, b.platform, b.external_id, b.cover, b.hero, b.total, b.last_unlock_at
FROM best_games b
WHERE b.user_id = sqlc.arg(user_id)::uuid AND b.total > 0 AND b.earned = b.total
  AND b.last_unlock_at >= sqlc.arg(since)::timestamptz AND b.last_unlock_at < sqlc.arg(until)::timestamptz
ORDER BY b.last_unlock_at;

-- name: RecapDays :many
-- Dias (Brasília) com desbloqueio e quantos — base da sequência e do dia da semana favorito.
SELECT (t.earned_at AT TIME ZONE 'America/Sao_Paulo')::date AS day, COUNT(*)::int AS n
FROM best_games b
JOIN trophies t ON t.game_id = b.id AND t.earned
WHERE b.user_id = sqlc.arg(user_id)::uuid
  AND t.earned_at >= sqlc.arg(since)::timestamptz AND t.earned_at < sqlc.arg(until)::timestamptz
GROUP BY 1
ORDER BY 1;

-- name: UpsertRecapPending :one
-- Cria (ou reabre, se deu erro) o resumo da semana. inserted = false → já existia pronto/pendente.
INSERT INTO ai_recaps (user_id, week, status) VALUES ($1, $2, 'pending')
ON CONFLICT (user_id, week) DO UPDATE SET status = 'pending', error = NULL, updated_at = now()
    WHERE ai_recaps.status = 'error'
RETURNING user_id, week, status, (xmax = 0) AS inserted;

-- name: GetRecap :one
SELECT * FROM ai_recaps WHERE user_id = $1 AND week = $2;

-- name: GetLatestRecap :one
SELECT * FROM ai_recaps WHERE user_id = $1 ORDER BY week DESC LIMIT 1;

-- name: CompleteRecap :exec
UPDATE ai_recaps SET status = 'ready', content = $3, model = $4, error = NULL, updated_at = now()
WHERE user_id = $1 AND week = $2;

-- name: FailRecap :exec
UPDATE ai_recaps SET status = 'error', error = $3, updated_at = now() WHERE user_id = $1 AND week = $2;

-- name: WeeklyRecapUsers :many
-- Quem ligou o resumo semanal (preferências em settings "notifications:<uuid>").
SELECT u.id
FROM users u
JOIN settings s ON s.key = 'notifications:' || u.id::text
WHERE u.status = 'active' AND (s.value ->> 'weekly_recap')::boolean IS TRUE;
