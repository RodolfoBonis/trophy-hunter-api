-- Web push: inscrições, avisos de desbloqueio ainda não enviados e anti-spam dos alertas de conta.

-- name: UpsertPushSubscription :one
INSERT INTO push_subscriptions (endpoint, p256dh, auth, user_agent)
VALUES ($1, $2, $3, $4)
ON CONFLICT (endpoint) DO UPDATE
SET p256dh = EXCLUDED.p256dh, auth = EXCLUDED.auth, user_agent = EXCLUDED.user_agent, failures = 0, updated_at = now()
RETURNING *;

-- name: DeletePushSubscriptionByEndpoint :execrows
DELETE FROM push_subscriptions WHERE endpoint = $1;

-- name: DeletePushSubscription :exec
DELETE FROM push_subscriptions WHERE id = $1;

-- name: ListPushSubscriptions :many
SELECT * FROM push_subscriptions ORDER BY created_at;

-- name: CountPushSubscriptions :one
SELECT count(*) FROM push_subscriptions;

-- name: MarkPushSuccess :exec
UPDATE push_subscriptions SET failures = 0, last_success_at = now(), updated_at = now() WHERE id = $1;

-- name: MarkPushFailure :one
UPDATE push_subscriptions SET failures = failures + 1, updated_at = now() WHERE id = $1 RETURNING failures;

-- name: ListUnpushedUnlocks :many
-- Desbloqueios ainda não notificados por push (mesmo formato dos avisos da UI).
SELECT e.id, t.name AS trophy_name, t.type AS trophy_type,
       g.id AS game_id, g.title AS game_title, g.platform,
       (CASE WHEN g.platform = 'psn' THEN t.type = 'platinum'
             ELSE NOT EXISTS (SELECT 1 FROM trophies o WHERE o.game_id = g.id AND NOT o.earned) END)::boolean AS completes_game
FROM unlock_events e
JOIN trophies t ON t.id = e.trophy_id
JOIN games g ON g.id = t.game_id
WHERE e.pushed_at IS NULL
ORDER BY e.detected_at, e.id
LIMIT 200;

-- name: MarkUnlocksPushed :exec
UPDATE unlock_events SET pushed_at = now() WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ClaimStatusNotification :one
-- Registra que o status `status` da conta vai ser notificado — só devolve a linha se for para notificar:
-- status mudou E (é needs_reconnect, que pede ação, OU o último aviso tem mais de 6 h). Evita spam de
-- uma conta oscilando entre erro e conectada.
UPDATE integrations
SET notified_status = sqlc.arg(status), notified_at = now()
WHERE id = sqlc.arg(id)
  AND notified_status <> sqlc.arg(status)
  AND (sqlc.arg(status) = 'needs_reconnect' OR notified_at IS NULL OR notified_at < now() - interval '6 hours')
RETURNING id, platform, label, display_name;

-- name: MarkStatusConnected :exec
-- Conta voltou ao normal: a próxima falha volta a poder notificar (respeitando a janela de 6 h).
UPDATE integrations SET notified_status = 'connected' WHERE id = $1 AND notified_status <> 'connected';
