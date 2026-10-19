-- Social (opt-in, recíproco): só entra quem está ativo E compartilha o progresso. Toda leitura de
-- outra pessoa passa por aqui; os handlers ainda exigem que QUEM PEDE também compartilhe.

-- name: SetShareProgress :one
UPDATE users SET share_progress = sqlc.arg(share)::boolean,
                 share_progress_at = CASE WHEN sqlc.arg(share)::boolean THEN now() ELSE NULL END,
                 updated_at = now()
WHERE id = sqlc.arg(id)::uuid
RETURNING *;

-- name: GetSharer :one
-- Um membro visível para os amigos (ativo e compartilhando).
SELECT * FROM users WHERE id = $1 AND status = 'active' AND share_progress;

-- name: Leaderboard :many
-- Placar dos membros que compartilham. Jogos contam uma vez (best_games = conta mais avançada).
-- since NULL = desde sempre; senão só desbloqueios a partir de since. psn_points: 15/30/90/300
-- (bronze/prata/ouro/platina) só da PSN; completed = jogos fechados (platina/100%) no período.
SELECT u.id, u.name,
       (COUNT(t.id) FILTER (WHERE t.type = 'platinum'))::int                                   AS platinum,
       COUNT(t.id)::int                                                                        AS earned,
       (COUNT(t.id) FILTER (WHERE t.rarity_pct < 5))::int                                      AS rare,
       COALESCE(SUM(CASE WHEN b.platform = 'psn' THEN CASE t.type WHEN 'platinum' THEN 300 WHEN 'gold' THEN 90
                                                              WHEN 'silver' THEN 30 WHEN 'bronze' THEN 15 ELSE 0 END
                         ELSE 0 END), 0)::int                                                  AS psn_points,
       (SELECT COUNT(*) FROM best_games c
         WHERE c.user_id = u.id AND c.total > 0 AND c.earned = c.total
           AND (sqlc.narg(since)::timestamptz IS NULL OR c.last_unlock_at >= sqlc.narg(since)::timestamptz))::int AS completed,
       MAX(t.earned_at)::timestamptz                                                           AS last_unlock_at
FROM users u
LEFT JOIN best_games b ON b.user_id = u.id
LEFT JOIN trophies t ON t.game_id = b.id AND t.earned
     AND (sqlc.narg(since)::timestamptz IS NULL OR t.earned_at >= sqlc.narg(since)::timestamptz)
WHERE u.status = 'active' AND u.share_progress
GROUP BY u.id, u.name
ORDER BY 3 DESC, 7 DESC, 4 DESC, u.name;

-- name: SocialFeed :many
-- Momentos dos membros que compartilham: platinas, jogos fechados e troféus raros (< 5%), do mais
-- recente, com cursor (earned_at, id). Todas as contas (game_progress), como a linha do tempo.
SELECT t.id, t.name, t.description, t.type, t.hidden, t.icon, t.rarity_pct, t.earned_at,
       gp.id AS game_id, gp.title AS game_title, gp.platform, gp.external_id, gp.cover, gp.hero,
       u.id AS user_id, u.name AS user_name,
       (gp.total > 0 AND gp.earned = gp.total AND t.earned_at = gp.last_unlock_at)::boolean AS completes_game
FROM trophies t
JOIN game_progress gp ON gp.id = t.game_id
JOIN users u ON u.id = gp.user_id
WHERE u.status = 'active' AND u.share_progress AND t.earned AND t.earned_at IS NOT NULL
  AND (t.type = 'platinum' OR t.rarity_pct < 5
       OR (gp.total > 0 AND gp.earned = gp.total AND t.earned_at = gp.last_unlock_at))
  AND (sqlc.narg(before_at)::timestamptz IS NULL
       OR (t.earned_at, t.id) < (sqlc.narg(before_at)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY t.earned_at DESC, t.id DESC
LIMIT sqlc.arg(lim);

-- name: GameFriends :many
-- Os outros membros que compartilham e têm o mesmo jogo — mesma plataforma+id OU mesmo título
-- normalizado (Steam × PSN). Uma linha por pessoa e plataforma (conta mais avançada).
SELECT u.id AS user_id, u.name AS user_name,
       b.id AS game_id, b.platform, b.earned, b.total, b.completion_pct, b.last_unlock_at,
       EXISTS (SELECT 1 FROM trophies p WHERE p.game_id = b.id AND p.type = 'platinum' AND p.earned)::boolean AS has_platinum
FROM best_games b
JOIN games g ON g.id = b.id
JOIN users u ON u.id = b.user_id
WHERE u.status = 'active' AND u.share_progress AND u.id <> sqlc.arg(viewer_id)::uuid
  AND b.total > 0
  AND ((b.platform = sqlc.arg(platform)::text AND b.external_id = sqlc.arg(external_id)::text)
       OR (sqlc.arg(normalized_title)::text <> '' AND g.normalized_title = sqlc.arg(normalized_title)::text))
ORDER BY b.completion_pct DESC, b.earned DESC, u.name;
