-- name: StartSyncRun :one
-- Cria o run (pai no fan-out, ou run único de um jogo). games_total/fanout_sealed já podem vir
-- preenchidos (run de jogo único: total=1, sealed=true); no fan-out de plataforma começam 0/false.
INSERT INTO sync_runs (integration_id, platform, scope, trigger, status, games_total, fanout_sealed)
VALUES ($1, $2, $3, $4, 'running', sqlc.arg(games_total)::int, sqlc.arg(sealed)::boolean)
RETURNING *;

-- name: SealSyncRun :one
-- Fixa games_total e marca o fan-out concluído. Se todos os filhos já terminaram (corrida com
-- filhos rápidos), fecha o run aqui mesmo. Devolve se fechou e quantos jogos falharam.
UPDATE sync_runs
SET games_total   = sqlc.arg(games_total)::int,
    fanout_sealed = true,
    status = CASE WHEN games_done >= sqlc.arg(games_total)::int
                  THEN CASE WHEN games_failed > 0 THEN 'error' ELSE 'ok' END
                  ELSE status END,
    finished_at = CASE WHEN games_done >= sqlc.arg(games_total)::int THEN now() ELSE finished_at END
WHERE id = $1 AND status = 'running'
RETURNING (games_done >= games_total)::boolean AS closed, games_failed, scope, integration_id;

-- name: FinalizeSyncRunChild :one
-- Contabiliza o término (terminal) de UM job filho. Fecha o run quando o fan-out está selado e
-- todos os filhos terminaram. Devolve se fechou e o total de falhas (para o status da integração).
UPDATE sync_runs
SET games_done    = games_done + 1,
    games_failed  = games_failed + sqlc.arg(failed)::int,
    items_changed = items_changed + sqlc.arg(changed)::int,
    status = CASE WHEN fanout_sealed AND games_done + 1 >= games_total
                  THEN CASE WHEN games_failed + sqlc.arg(failed)::int > 0 THEN 'error' ELSE 'ok' END
                  ELSE status END,
    finished_at = CASE WHEN fanout_sealed AND games_done + 1 >= games_total THEN now() ELSE finished_at END
WHERE id = $1 AND status = 'running'
RETURNING (fanout_sealed AND games_done >= games_total)::boolean AS closed, games_failed, scope, integration_id;

-- name: FailSyncRun :exec
-- Fecha o run como erro (ex.: falha ao listar a biblioteca, antes do fan-out).
UPDATE sync_runs
SET status = 'error', finished_at = now(), error = sqlc.arg(error)::text
WHERE id = $1 AND status = 'running';

-- name: ReapStuckSyncRuns :execrows
-- Reaper: runs presos em 'running' há mais de N minutos SEM nenhum job vivo que os referencie
-- (worker reiniciou no meio) → marca 'error' (interrompido). Roda no boot e periodicamente.
UPDATE sync_runs r
SET status = 'error', finished_at = now(),
    error = CASE WHEN r.error = '' THEN 'sync interrompido (worker reiniciou)' ELSE r.error END
WHERE r.status = 'running'
  AND r.started_at < now() - make_interval(mins => sqlc.arg(older_than_minutes)::int)
  AND NOT EXISTS (
      SELECT 1 FROM jobs j
      WHERE j.status IN ('queued','running')
        AND (j.payload->>'sync_run_id') = r.id::text
  );

-- name: ListSyncRuns :many
SELECT * FROM sync_runs
WHERE integration_id IN (SELECT i.id FROM integrations i WHERE i.user_id = sqlc.arg(user_id)::uuid)
  AND (sqlc.narg(platform)::text IS NULL OR platform = sqlc.narg(platform)::text)
  AND (sqlc.narg(integration_id)::uuid IS NULL OR integration_id = sqlc.narg(integration_id)::uuid)
ORDER BY started_at DESC
LIMIT $1;

-- name: DeleteSyncRun :exec
-- Remove um run órfão (ex.: enfileiramento suprimido por dedupe após criar o run).
DELETE FROM sync_runs WHERE id = $1;

-- name: CountJobsByKindStatus :many
-- Retrato da fila para a métrica jobs_queue (done não interessa: só o que espera, roda ou morreu).
SELECT kind, status, count(*) AS n FROM jobs
WHERE status IN ('queued', 'running', 'dead')
GROUP BY kind, status;
