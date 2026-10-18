-- Busca da paleta de comandos (⌘K): jogos e troféus. Só a conta mais avançada de cada jogo
-- (best_games), para não repetir o mesmo jogo/troféu por conta. Troféus ocultos ainda não obtidos
-- não aparecem (spoiler guard).

-- name: SearchGames :many
SELECT b.id, b.title, b.platform, b.external_id, b.cover, b.hero, b.completion_pct
FROM best_games b
JOIN games g ON g.id = b.id
WHERE b.user_id = sqlc.arg(user_id)::uuid AND g.normalized_title ILIKE '%' || sqlc.arg(q)::text || '%' ESCAPE '\'
ORDER BY similarity(g.normalized_title, sqlc.arg(q)::text) DESC, b.title
LIMIT 8;

-- name: SearchTrophies :many
SELECT t.id, t.name, t.type, t.icon, t.earned, t.rarity_pct,
       b.id AS game_id, b.title AS game_title, b.platform
FROM trophies t
JOIN best_games b ON b.id = t.game_id
WHERE b.user_id = sqlc.arg(user_id)::uuid AND t.name ILIKE '%' || sqlc.arg(q)::text || '%' ESCAPE '\'
  AND (t.earned OR NOT t.hidden)
ORDER BY similarity(lower(t.name), lower(sqlc.arg(q)::text)) DESC, t.name
LIMIT 12;
