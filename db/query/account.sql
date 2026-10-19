-- Dados do próprio usuário (LGPD): exportar tudo e excluir a conta.
-- Cada seção do export é montada no Postgres com colunas EXPLÍCITAS — credenciais (credentials_enc,
-- chaves de push, sessão do Claude) nunca entram, nem por engano de um SELECT *.

-- name: ExportIntegrations :one
SELECT coalesce(jsonb_agg(jsonb_build_object(
    'platform', i.platform, 'account_id', i.account_id, 'display_name', i.display_name, 'label', i.label,
    'status', i.status, 'last_synced_at', i.last_synced_at, 'created_at', i.created_at
) ORDER BY i.created_at), '[]')::jsonb AS data
FROM integrations i WHERE i.user_id = $1;

-- name: ExportGames :one
SELECT coalesce(jsonb_agg(jsonb_build_object(
    'platform', g.platform, 'external_id', g.external_id, 'title', g.title, 'account', i.display_name,
    'playtime_minutes', g.playtime_minutes, 'last_played_at', g.last_played_at,
    'abandoned', g.abandoned, 'priority', g.priority, 'notes', g.notes, 'tags', g.tags
) ORDER BY g.title), '[]')::jsonb AS data
FROM games g LEFT JOIN integrations i ON i.id = g.integration_id
WHERE g.user_id = $1;

-- name: ExportTrophies :one
SELECT coalesce(jsonb_agg(jsonb_build_object(
    'game_platform', g.platform, 'game_external_id', g.external_id, 'game_title', g.title,
    'external_id', t.external_id, 'name', t.name, 'type', t.type, 'rarity_pct', t.rarity_pct,
    'earned', t.earned, 'earned_at', t.earned_at, 'progress', t.progress, 'target', t.target,
    'priority', t.priority, 'notes', t.notes, 'tags', t.tags, 'server_shutdown_risk', t.server_shutdown_risk
) ORDER BY g.title, t.external_id), '[]')::jsonb AS data
FROM trophies t JOIN games g ON g.id = t.game_id
WHERE g.user_id = $1;

-- name: ExportChats :one
SELECT coalesce(jsonb_agg(jsonb_build_object(
    'platform', c.platform, 'game_external_id', c.game_external_id, 'archived', c.archived, 'created_at', c.created_at,
    'messages', (SELECT coalesce(jsonb_agg(jsonb_build_object('role', m.role, 'content', m.content, 'created_at', m.created_at)
                 ORDER BY m.created_at), '[]') FROM chat_messages m WHERE m.thread_id = c.id)
) ORDER BY c.created_at), '[]')::jsonb AS data
FROM chat_threads c WHERE c.user_id = $1;

-- name: ExportAIGuides :one
-- Roadmaps do usuário + as edições dele (inclusive sobre guias de troféu compartilhados).
SELECT jsonb_build_object(
    'roadmaps', (SELECT coalesce(jsonb_agg(jsonb_build_object(
        'platform', g.platform, 'game_external_id', g.game_external_id, 'content', g.content, 'generated_at', g.generated_at
    ) ORDER BY g.generated_at), '[]') FROM ai_guides g WHERE g.user_id = $1 AND g.scope = 'roadmap'),
    'edits', (SELECT coalesce(jsonb_agg(jsonb_build_object(
        'scope', g.scope, 'platform', g.platform, 'game_external_id', g.game_external_id,
        'trophy_external_id', g.trophy_external_id, 'content', e.content, 'updated_at', e.updated_at
    ) ORDER BY e.updated_at), '[]') FROM ai_guide_edits e JOIN ai_guides g ON g.id = e.guide_id WHERE e.user_id = $1),
    'hunt_progress', (SELECT coalesce(jsonb_agg(jsonb_build_object(
        'platform', h.game_platform, 'game_external_id', h.game_external_id, 'stage', h.stage_idx, 'done_at', h.done_at
    ) ORDER BY h.done_at), '[]') FROM hunt_progress h WHERE h.user_id = $1),
    'badges', (SELECT coalesce(jsonb_agg(jsonb_build_object('badge', b.badge_key, 'season', b.season, 'earned_at', b.earned_at)
        ORDER BY b.earned_at), '[]') FROM user_badges b WHERE b.user_id = $1),
    'wishlist', (SELECT coalesce(jsonb_agg(jsonb_build_object(
        'platform', w.platform, 'store_id', w.store_id, 'title', w.title, 'target_price_cents', w.target_price_cents, 'added_at', w.added_at
    ) ORDER BY w.added_at), '[]') FROM wishlist w WHERE w.user_id = $1),
    'weekly_recaps', (SELECT coalesce(jsonb_agg(jsonb_build_object('week', r.week, 'content', r.content) ORDER BY r.week), '[]')
        FROM ai_recaps r WHERE r.user_id = $1 AND r.status = 'ready')
)::jsonb AS data;

-- name: ExportChallenges :one
SELECT coalesce(jsonb_agg(jsonb_build_object(
    'title', c.title, 'kind', c.kind, 'game_title', c.game_title, 'starts_at', c.starts_at, 'ends_at', c.ends_at,
    'status', c.status, 'created_by_me', c.creator_id = $1, 'won', c.winner_id = $1, 'my_state', p.state
) ORDER BY c.created_at), '[]')::jsonb AS data
FROM challenges c JOIN challenge_participants p ON p.challenge_id = c.id AND p.user_id = $1;

-- name: DeleteUserLeftovers :exec
-- Depois do DeleteMember (cascade): o que não tem FK para users e sobraria: preferências e o histórico de sync de contas já apagadas.
WITH s AS (DELETE FROM settings WHERE key = 'notifications:' || @user_id::text)
DELETE FROM sync_runs WHERE integration_id IS NULL;
