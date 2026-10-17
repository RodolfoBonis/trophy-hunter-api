-- +goose Up
-- Progresso por linha de jogo (uma por conta) e a regra "conta mais avançada" num lugar só.
-- best_games = 1 linha por jogo (platform + external_id): a conta com maior % concluído; empate →
-- mais troféus ganhos → desbloqueio mais recente. É a base das estatísticas (somam as contas, mas
-- cada jogo conta uma vez) e do ranking Próxima Platina. A biblioteca usa o mesmo critério.
CREATE VIEW game_progress AS
SELECT g.id, g.integration_id, g.platform, g.external_id, g.title, g.cover, g.hero,
       g.has_achievements, g.abandoned, g.priority, g.tags, g.last_played_at,
       COUNT(t.id)::int                                AS total,
       (COUNT(t.id) FILTER (WHERE t.earned))::int      AS earned,
       MAX(t.earned_at)::timestamptz                   AS last_unlock_at,
       (CASE WHEN COUNT(t.id) > 0
             THEN (COUNT(t.id) FILTER (WHERE t.earned))::float8 / COUNT(t.id)
             ELSE 0 END)::float8                       AS completion_pct
FROM games g
LEFT JOIN trophies t ON t.game_id = g.id
GROUP BY g.id;

CREATE VIEW best_games AS
SELECT *
FROM (
    SELECT gp.*,
           ROW_NUMBER() OVER (PARTITION BY gp.platform, gp.external_id
                              ORDER BY gp.completion_pct DESC, gp.earned DESC,
                                       gp.last_unlock_at DESC NULLS LAST, gp.id) AS account_rank
    FROM game_progress gp
) ranked
WHERE account_rank = 1;

-- +goose Down
DROP VIEW best_games;
DROP VIEW game_progress;
