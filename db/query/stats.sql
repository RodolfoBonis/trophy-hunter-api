-- Estatísticas, ranking e linha do tempo. Tudo que é "por jogo" usa best_games (conta mais
-- avançada, ver migration 00015). Desbloqueios (recentes/linha do tempo) são de todas as contas.

-- name: StatsGames :one
SELECT
    COUNT(*)::int                                                                       AS total,
    (COUNT(*) FILTER (WHERE NOT abandoned AND earned = total))::int                      AS completed,
    (COUNT(*) FILTER (WHERE abandoned))::int                                            AS abandoned,
    (COUNT(*) FILTER (WHERE NOT abandoned AND earned < total AND completion_pct >= sqlc.arg(almost)::float8))::int AS almost,
    (COUNT(*) FILTER (WHERE NOT abandoned AND earned > 0 AND completion_pct < sqlc.arg(almost)::float8))::int      AS in_progress,
    (COUNT(*) FILTER (WHERE NOT abandoned AND earned = 0))::int                          AS not_started
FROM best_games
WHERE has_achievements AND total > 0;

-- name: StatsTrophies :one
SELECT
    COUNT(t.id)::int                                                                   AS total,
    (COUNT(t.id) FILTER (WHERE t.earned))::int                                         AS earned,
    (COUNT(t.id) FILTER (WHERE t.type = 'platinum'))::int                              AS platinum_total,
    (COUNT(t.id) FILTER (WHERE t.type = 'platinum' AND t.earned))::int                 AS platinum_earned,
    (COUNT(t.id) FILTER (WHERE t.type = 'gold'))::int                                  AS gold_total,
    (COUNT(t.id) FILTER (WHERE t.type = 'gold' AND t.earned))::int                     AS gold_earned,
    (COUNT(t.id) FILTER (WHERE t.type = 'silver'))::int                                AS silver_total,
    (COUNT(t.id) FILTER (WHERE t.type = 'silver' AND t.earned))::int                   AS silver_earned,
    (COUNT(t.id) FILTER (WHERE t.type = 'bronze'))::int                                AS bronze_total,
    (COUNT(t.id) FILTER (WHERE t.type = 'bronze' AND t.earned))::int                   AS bronze_earned,
    (COUNT(t.id) FILTER (WHERE t.earned AND t.rarity_pct < 1))::int                    AS legendary_earned,
    (COUNT(t.id) FILTER (WHERE t.earned AND t.rarity_pct >= 1 AND t.rarity_pct < 5))::int   AS epic_earned,
    (COUNT(t.id) FILTER (WHERE t.earned AND t.rarity_pct >= 5 AND t.rarity_pct < 20))::int  AS rare_earned,
    (COUNT(t.id) FILTER (WHERE t.earned AND t.rarity_pct >= 20 AND t.rarity_pct < 50))::int AS uncommon_earned,
    (COUNT(t.id) FILTER (WHERE t.earned AND t.rarity_pct >= 50))::int                  AS common_earned
FROM best_games b
JOIN trophies t ON t.game_id = b.id;

-- name: RarestEarned :many
-- As conquistas/troféus ganhos mais raros (menor % de jogadores).
SELECT t.id, t.name, t.type, t.icon, t.rarity_pct::float8 AS rarity_pct, t.earned_at,
       b.id AS game_id, b.title AS game_title, b.platform
FROM best_games b
JOIN trophies t ON t.game_id = b.id
WHERE t.earned AND t.rarity_pct IS NOT NULL
ORDER BY t.rarity_pct ASC, t.earned_at DESC
LIMIT sqlc.arg(lim);

-- name: RecentCompletions :many
-- Jogos completados (platina / 100%), do mais recente; a data é o último desbloqueio.
SELECT b.id, b.title, b.platform, b.external_id, b.cover, b.hero, b.total, b.last_unlock_at
FROM best_games b
WHERE b.total > 0 AND b.earned = b.total
ORDER BY b.last_unlock_at DESC NULLS LAST
LIMIT sqlc.arg(lim);

-- name: Evolution :many
-- Desbloqueios por mês (jogos contados pela conta mais avançada), para o gráfico de evolução.
SELECT to_char(date_trunc('month', t.earned_at), 'YYYY-MM') AS month, COUNT(*)::int AS earned
FROM best_games b
JOIN trophies t ON t.game_id = b.id
WHERE t.earned AND t.earned_at IS NOT NULL
GROUP BY 1
ORDER BY 1;

-- name: Timeline :many
-- Linha do tempo: desbloqueios de TODAS as contas, do mais recente, com paginação por cursor
-- (earned_at, id). completes_game = este desbloqueio fechou o jogo (platina/100%) naquela conta.
SELECT t.id, t.name, t.description, t.type, t.hidden, t.icon, t.rarity_pct, t.earned_at,
       gp.id AS game_id, gp.title AS game_title, gp.platform, gp.external_id, gp.cover, gp.hero,
       i.label AS account_label,
       (gp.total > 0 AND gp.earned = gp.total AND t.earned_at = gp.last_unlock_at)::boolean AS completes_game
FROM trophies t
JOIN game_progress gp ON gp.id = t.game_id
JOIN integrations i ON i.id = gp.integration_id
WHERE t.earned AND t.earned_at IS NOT NULL
  AND (sqlc.narg(before_at)::timestamptz IS NULL
       OR (t.earned_at, t.id) < (sqlc.narg(before_at)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY t.earned_at DESC, t.id DESC
LIMIT sqlc.arg(lim);

-- name: NextPlatinumCandidates :many
-- Candidatos à "Próxima Platina": jogos começados e não concluídos/abandonados (conta mais
-- avançada), com o que falta: quantidade, raridade média do que falta (maior % = mais fácil) e
-- quantos dos que faltam estão marcados como perdíveis/online.
SELECT b.id, b.integration_id, b.platform, b.external_id, b.title, b.cover, b.hero, b.priority,
       b.total, b.earned, b.completion_pct, b.last_unlock_at, b.last_played_at,
       (COUNT(t.id) FILTER (WHERE NOT t.earned))::int                                         AS remaining,
       COALESCE(AVG(t.rarity_pct) FILTER (WHERE NOT t.earned), 0)::float8                      AS avg_remaining_rarity,
       COALESCE(MIN(t.rarity_pct) FILTER (WHERE NOT t.earned), 0)::float8                      AS min_remaining_rarity,
       (COUNT(t.id) FILTER (WHERE NOT t.earned AND 'missable' = ANY(t.tags)))::int             AS remaining_missable,
       (COUNT(t.id) FILTER (WHERE NOT t.earned AND ('online' = ANY(t.tags) OR t.server_shutdown_risk)))::int AS remaining_online
FROM best_games b
JOIN trophies t ON t.game_id = b.id
WHERE NOT b.abandoned AND b.has_achievements AND b.total > 0 AND b.earned > 0 AND b.earned < b.total
GROUP BY b.id, b.integration_id, b.platform, b.external_id, b.title, b.cover, b.hero, b.priority,
         b.total, b.earned, b.completion_pct, b.last_unlock_at, b.last_played_at;

-- name: EasiestRemaining :many
-- Os troféus que faltam mais "fáceis" de um jogo (maior % de jogadores que têm) — "o que fazer hoje".
SELECT t.id, t.name, t.description, t.type, t.hidden, t.icon, t.icon_locked, t.rarity_pct, t.tags
FROM trophies t
WHERE t.game_id = $1 AND NOT t.earned
ORDER BY t.rarity_pct DESC NULLS LAST, t.name
LIMIT sqlc.arg(lim);

-- name: Alerts :many
-- Alertas: troféus que faltam marcados como perdíveis/online/dificuldade (ou com risco de servidor
-- fechar) em jogos não concluídos e não abandonados.
SELECT t.id, t.name, t.type, t.hidden, t.icon, t.rarity_pct, t.tags, t.server_shutdown_risk,
       gp.id AS game_id, gp.title AS game_title, gp.platform
FROM trophies t
JOIN game_progress gp ON gp.id = t.game_id
WHERE NOT t.earned AND NOT gp.abandoned AND gp.earned < gp.total
  AND (t.tags && ARRAY['missable','online','difficulty']::text[] OR t.server_shutdown_risk)
ORDER BY t.server_shutdown_risk DESC, gp.title, t.name
LIMIT 200;

-- name: UpdateGameAnnotations :one
-- Marcações do usuário no jogo (só os campos enviados mudam).
UPDATE games SET
    abandoned  = COALESCE(sqlc.narg(abandoned)::boolean, abandoned),
    priority   = COALESCE(sqlc.narg(priority)::int, priority),
    notes      = COALESCE(sqlc.narg(notes)::text, notes),
    tags       = COALESCE(sqlc.narg(tags)::text[], tags),
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: UpdateTrophyAnnotations :one
-- Marcações do usuário no troféu (só os campos enviados mudam).
UPDATE trophies SET
    priority             = COALESCE(sqlc.narg(priority)::int, priority),
    notes                = COALESCE(sqlc.narg(notes)::text, notes),
    tags                 = COALESCE(sqlc.narg(tags)::text[], tags),
    server_shutdown_risk = COALESCE(sqlc.narg(server_shutdown_risk)::boolean, server_shutdown_risk),
    updated_at           = now()
WHERE id = sqlc.arg(id)
RETURNING *;
