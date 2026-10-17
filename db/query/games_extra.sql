-- name: SetGameHasAchievements :exec
UPDATE games SET has_achievements = $2, updated_at = now() WHERE id = $1;
