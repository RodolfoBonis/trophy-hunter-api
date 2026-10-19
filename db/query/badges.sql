-- name: BadgeStats :one
-- Agregados das medalhas (melhor conta por jogo). "Platina" do app = platina PSN ou 100% na Steam.
WITH e AS (
    SELECT t.type, t.rarity_pct, t.earned_at
    FROM best_games b JOIN trophies t ON t.game_id = b.id AND t.earned
    WHERE b.user_id = sqlc.arg(user_id)::uuid
), g AS (
    SELECT b.platform, (b.total > 0 AND b.earned = b.total) AS complete
    FROM best_games b WHERE b.user_id = sqlc.arg(user_id)::uuid
)
SELECT
    (SELECT count(*) FROM e)::int                                                        AS earned,
    (SELECT count(*) FROM e WHERE type = 'platinum')::int                                AS psn_platinum,
    (SELECT count(*) FROM e WHERE rarity_pct < 5)::int                                   AS rare,
    (SELECT count(*) FROM e WHERE rarity_pct < 1)::int                                   AS ultra_rare,
    (SELECT count(*) FROM e WHERE extract(hour FROM earned_at AT TIME ZONE 'America/Sao_Paulo') < 5)::int AS night,
    (SELECT count(*) FROM g WHERE complete)::int                                         AS completed,
    (SELECT count(*) FROM g WHERE complete AND platform = 'steam')::int                  AS steam_completed,
    (SELECT count(*) FROM challenges c WHERE c.winner_id = sqlc.arg(user_id)::uuid)::int AS challenges_won;

-- name: SeasonStats :one
-- Mês da temporada: platinas PSN + jogos Steam fechados no período, e raros do período.
SELECT
    (SELECT count(*) FROM best_games b JOIN trophies t ON t.game_id = b.id AND t.earned
     WHERE b.user_id = sqlc.arg(user_id)::uuid AND t.type = 'platinum'
       AND t.earned_at >= sqlc.arg(since)::timestamptz AND t.earned_at < sqlc.arg(until)::timestamptz)::int
  + (SELECT count(*) FROM best_games b
     WHERE b.user_id = sqlc.arg(user_id)::uuid AND b.platform = 'steam' AND b.total > 0 AND b.earned = b.total
       AND b.last_unlock_at >= sqlc.arg(since)::timestamptz AND b.last_unlock_at < sqlc.arg(until)::timestamptz)::int AS platinums,
    (SELECT count(*) FROM best_games b JOIN trophies t ON t.game_id = b.id AND t.earned
     WHERE b.user_id = sqlc.arg(user_id)::uuid AND t.rarity_pct < 5
       AND t.earned_at >= sqlc.arg(since)::timestamptz AND t.earned_at < sqlc.arg(until)::timestamptz)::int AS rare;

-- name: ListUserBadges :many
SELECT badge_key, season, earned_at FROM user_badges WHERE user_id = $1 ORDER BY earned_at DESC, badge_key;

-- name: CountUserBadges :one
SELECT count(*)::int FROM user_badges WHERE user_id = $1;

-- name: AwardBadge :one
-- inserted = false → já tinha.
INSERT INTO user_badges (user_id, badge_key, season) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING
RETURNING (xmax = 0) AS inserted;

-- name: ActiveUserIDs :many
SELECT id FROM users WHERE status = 'active';
