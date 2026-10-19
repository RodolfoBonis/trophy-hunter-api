-- name: ListHuntStagesDone :many
SELECT stage_idx FROM hunt_progress
WHERE user_id = $1 AND game_platform = $2 AND game_external_id = $3
ORDER BY stage_idx;

-- name: MarkHuntStage :exec
INSERT INTO hunt_progress (user_id, game_platform, game_external_id, stage_idx)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: UnmarkHuntStage :exec
DELETE FROM hunt_progress
WHERE user_id = $1 AND game_platform = $2 AND game_external_id = $3 AND stage_idx = $4;

-- name: ResetHuntStagesFrom :exec
-- Roadmap novo/editado com menos etapas: some o que apontava para etapa que não existe mais.
DELETE FROM hunt_progress
WHERE user_id = $1 AND game_platform = $2 AND game_external_id = $3 AND stage_idx >= sqlc.arg(stage_count)::int;

-- name: GameSyncInFlight :one
-- Há sync deste jogo na fila ou rodando? (o modo caçada acompanha enquanto for true)
SELECT EXISTS (
    SELECT 1 FROM jobs
    WHERE kind = 'sync_game' AND status IN ('queued', 'running') AND payload->>'game_id' = sqlc.arg(game_id)::text
)::boolean AS syncing;
