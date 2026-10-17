-- name: GetGameByID :one
SELECT * FROM games WHERE id = $1;

-- name: GetGameByExternal :one
SELECT * FROM games WHERE integration_id = $1 AND external_id = $2;

-- name: UpsertGameIdentity :one
-- Upserta a IDENTIDADE e os campos de EXIBIÇÃO do jogo (title, icon, cover, playtime, last_played),
-- preservando campos do usuário (abandoned, priority, notes, tags, unified_game_id).
--
-- NÃO sobrescreve, no conflito:
--   - has_achievements: quem decide é o sync de schema (true só quando há conquistas; ver
--     SetGameHasAchievements). Isso evita voltar has_achievements para true via has_community_visible
--     depois de um schema vazio/ErrNoStats o ter zerado.
--   - remote_updated_at: é a MARCA D'ÁGUA do sync incremental, gravada só APÓS as conquistas do
--     jogo sincronizarem com sucesso (SetGameWatermark). Um jogo que falhou mantém a marca antiga
--     e é re-sincronizado no próximo incremental.
-- Em INSERT (jogo novo), has_achievements vem do parâmetro (has_community_visible_stats).
INSERT INTO games (
    integration_id, platform, external_id, np_service_name, title, normalized_title, icon, cover,
    hero, platforms, has_achievements, last_played_at, playtime_minutes, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now()
)
ON CONFLICT (integration_id, external_id) DO UPDATE SET
    np_service_name   = EXCLUDED.np_service_name,
    title             = EXCLUDED.title,
    normalized_title  = EXCLUDED.normalized_title,
    icon              = EXCLUDED.icon,
    cover             = EXCLUDED.cover,
    hero              = EXCLUDED.hero,
    platforms         = EXCLUDED.platforms,
    last_played_at    = EXCLUDED.last_played_at,
    playtime_minutes  = EXCLUDED.playtime_minutes,
    updated_at        = now()
RETURNING *;

-- name: SetGameWatermark :exec
-- Grava a marca d'água do sync incremental (remote_updated_at) APÓS o sync de conquistas do jogo
-- ter sucesso. Ver UpsertGameIdentity.
UPDATE games SET remote_updated_at = $2, updated_at = now() WHERE id = $1;

-- name: SetGameSchemaSynced :exec
UPDATE games SET schema_synced_at = now(), has_platinum = $2, updated_at = now() WHERE id = $1;

-- name: SetGamePercentagesSynced :exec
UPDATE games SET percentages_synced_at = now(), updated_at = now() WHERE id = $1;

-- A biblioteca agrupa o MESMO jogo (platform + external_id) de várias contas em UMA entrada,
-- representada pela conta MAIS AVANÇADA ("best"): maior % concluído; empate → mais troféus
-- ganhos; empate → desbloqueio mais recente. Filtros e ordenação valem sobre a linha best.
-- account_count = em quantas contas o jogo existe (no escopo do filtro de conta, se houver).
-- Esta é a mesma regra das estatísticas: somar contas, mas cada jogo conta uma vez (o best).

-- name: ListGames :many
WITH agg AS (
    SELECT g.*, i.label AS account_label,
        COUNT(t.id)                                                        AS total,
        COUNT(t.id) FILTER (WHERE t.earned)                                AS earned,
        COALESCE(AVG(t.rarity_pct) FILTER (WHERE NOT t.earned), 0)::float8  AS avg_remaining_rarity,
        MAX(t.earned_at)::timestamptz                                      AS last_unlock_at,
        -- Contagens por tipo (PSN) e por tier de raridade p/ o card.
        COUNT(t.id) FILTER (WHERE t.type = 'platinum')                     AS platinum_total,
        COUNT(t.id) FILTER (WHERE t.type = 'platinum' AND t.earned)        AS platinum_earned,
        COUNT(t.id) FILTER (WHERE t.type = 'gold')                         AS gold_total,
        COUNT(t.id) FILTER (WHERE t.type = 'gold' AND t.earned)            AS gold_earned,
        COUNT(t.id) FILTER (WHERE t.type = 'silver')                       AS silver_total,
        COUNT(t.id) FILTER (WHERE t.type = 'silver' AND t.earned)          AS silver_earned,
        COUNT(t.id) FILTER (WHERE t.type = 'bronze')                       AS bronze_total,
        COUNT(t.id) FILTER (WHERE t.type = 'bronze' AND t.earned)          AS bronze_earned,
        -- Ganhas por tier de raridade (mesmos cortes do front/style tile: <1, <5, <20, <50, resto).
        COUNT(t.id) FILTER (WHERE t.earned AND t.rarity_pct < 1)                          AS legendary_earned,
        COUNT(t.id) FILTER (WHERE t.earned AND t.rarity_pct >= 1 AND t.rarity_pct < 5)    AS epic_earned,
        COUNT(t.id) FILTER (WHERE t.earned AND t.rarity_pct >= 5 AND t.rarity_pct < 20)   AS rare_earned,
        COUNT(t.id) FILTER (WHERE t.earned AND t.rarity_pct >= 20 AND t.rarity_pct < 50)  AS uncommon_earned,
        COUNT(t.id) FILTER (WHERE t.earned AND t.rarity_pct >= 50)                        AS common_earned,
        -- Também existe em outra plataforma (unificado)?
        EXISTS (SELECT 1 FROM games o WHERE o.unified_game_id = g.unified_game_id AND o.platform <> g.platform) AS cross_platform,
        -- Um card por jogo: o unificado (Steam + PSN) ou, sem vínculo, a chave da plataforma.
        COALESCE(g.unified_game_id::text, g.platform || ':' || g.external_id)::text AS group_key
    FROM games g
    JOIN integrations i ON i.id = g.integration_id
    LEFT JOIN trophies t ON t.game_id = g.id
    WHERE (sqlc.narg(integration_id)::uuid IS NULL OR g.integration_id = sqlc.narg(integration_id)::uuid)
      AND (sqlc.narg(platform)::text IS NULL OR g.platform = sqlc.narg(platform)::text)
    GROUP BY g.id, i.label
),
scored AS (
    SELECT *,
        (total - earned)::bigint                                             AS remaining,
        (CASE WHEN total > 0 THEN earned::float8 / total ELSE 0 END)::float8 AS completion_pct,
        CASE
            WHEN abandoned                                      THEN 'abandoned'
            WHEN total > 0 AND earned = total                   THEN 'completed'
            WHEN total > 0 AND earned::float8 / total >= sqlc.arg(almost)::float8 THEN 'almost'
            WHEN earned > 0                                     THEN 'in_progress'
            ELSE 'not_started'
        END                                                                  AS status_bucket
    FROM agg
),
ranked AS (
    SELECT *,
        ROW_NUMBER() OVER w AS account_rank,
        COUNT(*) OVER (PARTITION BY platform, external_id) AS account_count
    FROM scored
    -- A conta (e, se unificado, a plataforma) mais avançada representa o jogo.
    WINDOW w AS (PARTITION BY group_key
                 ORDER BY completion_pct DESC, earned DESC, last_unlock_at DESC NULLS LAST, id)
)
SELECT * FROM ranked
WHERE account_rank = 1
  AND (sqlc.narg(has_achievements)::boolean IS NULL OR has_achievements = sqlc.narg(has_achievements)::boolean)
  AND (sqlc.narg(q)::text IS NULL OR normalized_title ILIKE '%' || sqlc.narg(q)::text || '%' ESCAPE '\')
  AND (sqlc.narg(status)::text IS NULL OR status_bucket = sqlc.narg(status)::text)
ORDER BY
    (CASE WHEN sqlc.arg(sort)::text = 'title'      THEN normalized_title     END) ASC  NULLS LAST,
    (CASE WHEN sqlc.arg(sort)::text = 'completion' THEN completion_pct       END) DESC NULLS LAST,
    (CASE WHEN sqlc.arg(sort)::text = 'remaining'  THEN remaining            END) ASC  NULLS LAST,
    (CASE WHEN sqlc.arg(sort)::text = 'rarity'     THEN avg_remaining_rarity END) DESC NULLS LAST,
    (CASE WHEN sqlc.arg(sort)::text NOT IN ('title','completion','remaining','rarity')
          THEN last_played_at END) DESC NULLS LAST,
    id
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: CountGames :one
WITH agg AS (
    SELECT g.id, g.platform, g.external_id, g.has_achievements, g.normalized_title, g.abandoned,
        COALESCE(g.unified_game_id::text, g.platform || ':' || g.external_id)::text AS group_key,
        COUNT(t.id)                         AS total,
        COUNT(t.id) FILTER (WHERE t.earned) AS earned,
        MAX(t.earned_at)::timestamptz       AS last_unlock_at
    FROM games g
    LEFT JOIN trophies t ON t.game_id = g.id
    WHERE (sqlc.narg(integration_id)::uuid IS NULL OR g.integration_id = sqlc.narg(integration_id)::uuid)
      AND (sqlc.narg(platform)::text IS NULL OR g.platform = sqlc.narg(platform)::text)
    GROUP BY g.id
),
scored AS (
    SELECT *,
        (CASE WHEN total > 0 THEN earned::float8 / total ELSE 0 END)::float8 AS completion_pct,
        CASE
            WHEN abandoned                                      THEN 'abandoned'
            WHEN total > 0 AND earned = total                   THEN 'completed'
            WHEN total > 0 AND earned::float8 / total >= sqlc.arg(almost)::float8 THEN 'almost'
            WHEN earned > 0                                     THEN 'in_progress'
            ELSE 'not_started'
        END AS status_bucket
    FROM agg
),
ranked AS (
    SELECT *,
        ROW_NUMBER() OVER (PARTITION BY group_key
                           ORDER BY completion_pct DESC, earned DESC, last_unlock_at DESC NULLS LAST, id) AS account_rank
    FROM scored
)
SELECT COUNT(*) FROM ranked
WHERE account_rank = 1
  AND (sqlc.narg(has_achievements)::boolean IS NULL OR has_achievements = sqlc.narg(has_achievements)::boolean)
  AND (sqlc.narg(q)::text IS NULL OR normalized_title ILIKE '%' || sqlc.narg(q)::text || '%' ESCAPE '\')
  AND (sqlc.narg(status)::text IS NULL OR status_bucket = sqlc.narg(status)::text);

-- name: ListGameAccounts :many
-- Todas as contas que possuem os jogos dados (chaves "platform:external_id"), com o progresso de
-- cada uma, já na ordem "mais avançada primeiro" (mesmo critério do best da biblioteca).
WITH agg AS (
    SELECT g.id, g.integration_id, g.platform, g.external_id, g.abandoned, i.label AS account_label,
        i.display_name AS account_display_name,
        COUNT(t.id)                         AS total,
        COUNT(t.id) FILTER (WHERE t.earned) AS earned,
        MAX(t.earned_at)::timestamptz       AS last_unlock_at,
        COALESCE(bool_or(t.type = 'platinum' AND t.earned), false)::boolean AS has_platinum_earned
    FROM games g
    JOIN integrations i ON i.id = g.integration_id
    LEFT JOIN trophies t ON t.game_id = g.id
    WHERE (g.platform || ':' || g.external_id) = ANY(sqlc.arg(keys)::text[])
    GROUP BY g.id, i.label, i.display_name
)
SELECT *,
    (CASE WHEN total > 0 THEN earned::float8 / total ELSE 0 END)::float8 AS completion_pct,
    CASE
        WHEN abandoned                                      THEN 'abandoned'
        WHEN total > 0 AND earned = total                   THEN 'completed'
        WHEN total > 0 AND earned::float8 / total >= sqlc.arg(almost)::float8 THEN 'almost'
        WHEN earned > 0                                     THEN 'in_progress'
        ELSE 'not_started'
    END AS status_bucket
FROM agg
ORDER BY platform, external_id,
    (CASE WHEN total > 0 THEN earned::float8 / total ELSE 0 END) DESC, earned DESC,
    last_unlock_at DESC NULLS LAST, id;
