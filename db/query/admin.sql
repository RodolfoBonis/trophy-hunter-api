-- Painel do dono (GET /admin/health). Só leitura; agregados pequenos.

-- name: AdminAccounts :many
SELECT i.id, u.name AS user_name, i.platform, i.display_name, i.label, i.status, i.last_synced_at, i.last_error
FROM integrations i JOIN users u ON u.id = i.user_id
ORDER BY u.name, i.platform, i.display_name;

-- name: AdminSyncRuns24h :many
SELECT status, count(*)::int AS n FROM sync_runs WHERE started_at >= now() - interval '24 hours' GROUP BY status;

-- name: AdminJobCounts :many
SELECT kind, status, count(*)::int AS n FROM jobs WHERE status IN ('queued', 'running', 'dead') GROUP BY kind, status ORDER BY kind, status;

-- name: AdminRecentDead :many
SELECT id, kind, attempts, coalesce(last_error, '')::text AS last_error, updated_at FROM jobs WHERE status = 'dead' ORDER BY updated_at DESC LIMIT 20;

-- name: AdminAIUsage :many
SELECT u.id, u.name, u.role,
       (count(r.id) FILTER (WHERE r.created_at >= now() - interval '7 days'))::int AS last_7d,
       (count(r.id) FILTER (WHERE r.created_at >= now() - interval '24 hours'))::int AS last_24h
FROM users u LEFT JOIN ai_requests r ON r.user_id = u.id
WHERE u.status = 'active'
GROUP BY u.id, u.name, u.role
ORDER BY last_7d DESC, u.name;

-- name: AdminTotals :one
SELECT
    (SELECT count(*) FROM users WHERE status = 'active')::int AS members,
    (SELECT count(*) FROM user_badges)::int AS badges,
    (SELECT count(*) FROM challenges WHERE status = 'open')::int AS open_challenges,
    (SELECT count(*) FROM wishlist)::int AS wishlist_items,
    (SELECT count(*) FROM users WHERE public_enabled)::int AS public_profiles;
