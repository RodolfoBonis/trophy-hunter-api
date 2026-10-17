-- name: StartSyncRun :one
INSERT INTO sync_runs (integration_id, platform, scope, trigger, status)
VALUES ($1, $2, $3, $4, 'running')
RETURNING *;

-- name: FinishSyncRun :exec
UPDATE sync_runs
SET status = $2, finished_at = now(), items_changed = $3, games_touched = $4, error = $5
WHERE id = $1;

-- name: ListSyncRuns :many
SELECT * FROM sync_runs
WHERE (sqlc.narg(platform)::text IS NULL OR platform = sqlc.narg(platform)::text)
ORDER BY started_at DESC
LIMIT $1;
