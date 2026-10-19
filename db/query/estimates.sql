-- Estimativas de tempo (HowLongToBeat), por título normalizado.

-- name: TitlesNeedingEstimate :many
-- Títulos sem estimativa, ou vencida (achou: 30 dias; não achou: 7 dias): jogos com conquistas e os
-- da lista de desejos (backlog). Os sem nenhuma tentativa e os mais recentes primeiro.
WITH titles AS (
    SELECT g.normalized_title, g.title, g.last_played_at AS recent
    FROM games g WHERE g.has_achievements AND g.normalized_title <> ''
    UNION ALL
    SELECT w.normalized_title, w.title, w.added_at AS recent
    FROM wishlist w WHERE w.normalized_title <> ''
)
SELECT t.normalized_title, MIN(t.title)::text AS title
FROM titles t
LEFT JOIN game_estimates e ON e.normalized_title = t.normalized_title
WHERE e.normalized_title IS NULL
   OR (e.status = 'found' AND e.fetched_at < now() - interval '30 days')
   OR (e.status = 'not_found' AND e.fetched_at < now() - interval '7 days')
GROUP BY t.normalized_title, e.normalized_title
ORDER BY (e.normalized_title IS NULL) DESC, MAX(t.recent) DESC NULLS LAST
LIMIT sqlc.arg(lim);

-- name: UpsertEstimate :exec
INSERT INTO game_estimates (normalized_title, status, hltb_id, hltb_name, main_h, extra_h, completionist_h, fetched_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now())
ON CONFLICT (normalized_title) DO UPDATE SET
    status = EXCLUDED.status, hltb_id = EXCLUDED.hltb_id, hltb_name = EXCLUDED.hltb_name,
    main_h = EXCLUDED.main_h, extra_h = EXCLUDED.extra_h, completionist_h = EXCLUDED.completionist_h,
    fetched_at = now();

-- name: GetEstimate :one
SELECT * FROM game_estimates WHERE normalized_title = $1 AND status = 'found';
