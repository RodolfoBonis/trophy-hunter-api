-- Desafios entre amigos.

-- name: CreateChallenge :one
INSERT INTO challenges (creator_id, kind, title, game_platform, game_external_id, normalized_title, game_title, game_cover, ends_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: AddChallengeParticipant :exec
INSERT INTO challenge_participants (challenge_id, user_id, state, responded_at)
VALUES ($1, $2, $3, CASE WHEN $3 = 'accepted' THEN now() END)
ON CONFLICT DO NOTHING;

-- name: GetChallengeForUser :one
-- O desafio, se quem pede participa (convidado, aceito ou recusou) — senão "não existe".
SELECT c.*, p.state AS my_state
FROM challenges c
JOIN challenge_participants p ON p.challenge_id = c.id AND p.user_id = sqlc.arg(user_id)::uuid
WHERE c.id = sqlc.arg(id)::uuid;

-- name: ListChallengesForUser :many
SELECT c.*, p.state AS my_state,
       (SELECT COUNT(*) FROM challenge_participants x WHERE x.challenge_id = c.id AND x.state = 'accepted')::int AS accepted_count,
       w.name AS winner_name
FROM challenges c
JOIN challenge_participants p ON p.challenge_id = c.id AND p.user_id = sqlc.arg(user_id)::uuid
LEFT JOIN users w ON w.id = c.winner_id
WHERE p.state <> 'declined' OR c.creator_id = sqlc.arg(user_id)::uuid
ORDER BY (c.status = 'open') DESC, c.created_at DESC
LIMIT 50;

-- name: RespondChallenge :one
UPDATE challenge_participants SET state = sqlc.arg(state)::text, responded_at = now()
WHERE challenge_id = sqlc.arg(challenge_id)::uuid AND user_id = sqlc.arg(user_id)::uuid AND state = 'invited'
RETURNING *;

-- name: CancelChallenge :execrows
UPDATE challenges SET status = 'canceled', finished_at = now()
WHERE id = $1 AND creator_id = $2 AND status = 'open';

-- name: ListOpenChallenges :many
SELECT * FROM challenges WHERE status = 'open' ORDER BY created_at;

-- name: FinishChallenge :execrows
UPDATE challenges SET status = 'finished', winner_id = sqlc.narg(winner_id)::uuid, finished_at = now()
WHERE id = sqlc.arg(id)::uuid AND status = 'open';

-- name: MarkParticipantCompleted :exec
UPDATE challenge_participants SET completed_at = $3 WHERE challenge_id = $1 AND user_id = $2 AND completed_at IS NULL;

-- name: SetParticipantMilestone :exec
UPDATE challenge_participants SET last_milestone = $3 WHERE challenge_id = $1 AND user_id = $2;

-- name: ChallengeParticipants :many
SELECT p.*, u.name AS user_name
FROM challenge_participants p JOIN users u ON u.id = p.user_id
WHERE p.challenge_id = $1
ORDER BY (p.state = 'accepted') DESC, u.name;

-- name: ChallengeGameProgress :one
-- Progresso de um participante no jogo do desafio (conta/plataforma mais avançada; mesmo jogo por
-- plataforma+id ou título normalizado). Sem o jogo → sem linha.
SELECT b.completion_pct, b.earned, b.total, b.last_unlock_at, b.platform
FROM best_games b
JOIN games g ON g.id = b.id
WHERE b.user_id = sqlc.arg(user_id)::uuid AND b.total > 0
  AND ((b.platform = sqlc.arg(platform)::text AND b.external_id = sqlc.arg(external_id)::text)
       OR g.normalized_title = sqlc.arg(normalized_title)::text)
ORDER BY b.completion_pct DESC, b.earned DESC
LIMIT 1;

-- name: ChallengeTrophyCount :one
-- Troféus obtidos na janela do desafio (jogos contados pela conta mais avançada); rare_only = só < 5%.
SELECT COUNT(t.id)::int AS n
FROM best_games b
JOIN trophies t ON t.game_id = b.id AND t.earned
WHERE b.user_id = sqlc.arg(user_id)::uuid
  AND t.earned_at >= sqlc.arg(since)::timestamptz
  AND t.earned_at < sqlc.arg(until)::timestamptz
  AND (NOT sqlc.arg(rare_only)::boolean OR t.rarity_pct < 5);

-- name: FriendGameMatch :one
-- A versão mais avançada do mesmo jogo de outra pessoa (plataforma+id ou título normalizado).
SELECT b.id, b.platform, b.external_id, b.earned, b.total, b.completion_pct
FROM best_games b
JOIN games g ON g.id = b.id
WHERE b.user_id = sqlc.arg(user_id)::uuid AND b.total > 0
  AND ((b.platform = sqlc.arg(platform)::text AND b.external_id = sqlc.arg(external_id)::text)
       OR (sqlc.arg(normalized_title)::text <> '' AND g.normalized_title = sqlc.arg(normalized_title)::text))
ORDER BY (b.platform = sqlc.arg(platform)::text AND b.external_id = sqlc.arg(external_id)::text) DESC,
         b.completion_pct DESC, b.earned DESC
LIMIT 1;
