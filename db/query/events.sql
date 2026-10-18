-- Eventos de desbloqueio (unlock_events): fonte dos avisos de troféu e da cerimônia de platina.

-- name: ListUnseenUnlocks :many
-- Desbloqueios ainda não exibidos, mais antigos primeiro. completes_game = o troféu fechou o jogo
-- (PSN: é a platina; Steam: com ele, não falta nenhuma conquista).
SELECT e.id, e.detected_at,
       t.id AS trophy_id, t.name AS trophy_name, t.description AS trophy_description, t.type AS trophy_type,
       t.icon AS trophy_icon, t.rarity_pct, t.earned_at,
       g.id AS game_id, g.title AS game_title, g.platform, g.external_id, g.cover, g.hero,
       (CASE WHEN g.platform = 'psn' THEN t.type = 'platinum'
             ELSE NOT EXISTS (SELECT 1 FROM trophies o WHERE o.game_id = g.id AND NOT o.earned) END)::boolean AS completes_game
FROM unlock_events e
JOIN trophies t ON t.id = e.trophy_id
JOIN games g ON g.id = t.game_id
WHERE e.seen_at IS NULL
ORDER BY e.detected_at, t.earned_at NULLS LAST, e.id
LIMIT 100;

-- name: MarkUnlocksSeen :execrows
UPDATE unlock_events SET seen_at = now() WHERE id = ANY(sqlc.arg(ids)::uuid[]) AND seen_at IS NULL;
