-- IA: guias (troféu/roadmap) e chat por jogo. Chave = platform + external_id do jogo (ver 00017).

-- name: GetTrophyGuide :one
-- Guia de troféu: compartilhado entre os usuários (user_id NULL).
SELECT * FROM ai_guides
WHERE scope = 'trophy' AND platform = $1 AND game_external_id = $2 AND trophy_external_id = $3;

-- name: GetRoadmap :one
-- Roadmap: de cada usuário (depende do progresso dele).
SELECT * FROM ai_guides
WHERE scope = 'roadmap' AND user_id = $1 AND platform = $2 AND game_external_id = $3;

-- name: GetGuideByID :one
SELECT * FROM ai_guides WHERE id = $1;

-- name: UpsertTrophyGuidePending :one
-- Cria (ou volta para pending ao regenerar) o guia compartilhado do troféu.
INSERT INTO ai_guides (scope, platform, game_external_id, trophy_external_id, status)
VALUES ('trophy', $1, $2, $3, 'pending')
ON CONFLICT (platform, game_external_id, trophy_external_id) WHERE scope = 'trophy'
DO UPDATE SET status = 'pending', error = '', updated_at = now()
RETURNING *;

-- name: UpsertRoadmapPending :one
INSERT INTO ai_guides (scope, user_id, platform, game_external_id, trophy_external_id, status)
VALUES ('roadmap', $1, $2, $3, '', 'pending')
ON CONFLICT (user_id, platform, game_external_id) WHERE scope = 'roadmap'
DO UPDATE SET status = 'pending', error = '', updated_at = now()
RETURNING *;

-- name: MarkGuideRunning :exec
UPDATE ai_guides SET status = 'running', updated_at = now() WHERE id = $1;

-- name: CompleteGuide :exec
UPDATE ai_guides
SET status = 'ready', content = $2, model = $3, schema_version = $4, error = '',
    generated_at = now(), updated_at = now()
WHERE id = $1;

-- name: FailGuide :exec
UPDATE ai_guides SET status = 'error', error = $2, updated_at = now() WHERE id = $1;

-- name: GetGuideEdit :one
-- Correção PRIVADA do usuário sobre um guia (vence o gerado só para ele).
SELECT content FROM ai_guide_edits WHERE user_id = $1 AND guide_id = $2;

-- name: UpsertGuideEdit :exec
INSERT INTO ai_guide_edits (user_id, guide_id, content) VALUES ($1, $2, $3)
ON CONFLICT (user_id, guide_id) DO UPDATE SET content = EXCLUDED.content, updated_at = now();

-- name: DeleteGuideEdit :exec
DELETE FROM ai_guide_edits WHERE user_id = $1 AND guide_id = $2;

-- name: GetActiveThread :one
SELECT * FROM chat_threads WHERE user_id = $1 AND platform = $2 AND game_external_id = $3 AND NOT archived;

-- name: GetThreadByID :one
SELECT * FROM chat_threads WHERE id = $1;

-- name: CreateThread :one
INSERT INTO chat_threads (user_id, platform, game_external_id) VALUES ($1, $2, $3)
ON CONFLICT (user_id, platform, game_external_id) WHERE NOT archived DO UPDATE SET updated_at = now()
RETURNING *;

-- name: ArchiveThread :exec
UPDATE chat_threads SET archived = true, updated_at = now()
WHERE user_id = $1 AND platform = $2 AND game_external_id = $3 AND NOT archived;

-- name: SetThreadSession :exec
UPDATE chat_threads SET claude_session_id = $2, updated_at = now() WHERE id = $1;

-- name: ListThreadMessages :many
SELECT * FROM chat_messages WHERE thread_id = $1 ORDER BY created_at, id;

-- name: InsertChatMessage :one
INSERT INTO chat_messages (thread_id, role, content, status) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetChatMessage :one
SELECT * FROM chat_messages WHERE id = $1;

-- name: GetChatMessageOwner :one
-- Dono da mensagem (para a stream SSE só servir a resposta a quem perguntou).
SELECT th.user_id, m.role, m.status, m.error
FROM chat_messages m JOIN chat_threads th ON th.id = m.thread_id
WHERE m.id = $1;

-- name: SetChatMessageRunning :exec
UPDATE chat_messages SET status = 'running', updated_at = now() WHERE id = $1;

-- name: CompleteChatMessage :exec
UPDATE chat_messages SET status = 'done', content = $2, error = '', updated_at = now() WHERE id = $1;

-- name: FailChatMessage :exec
UPDATE chat_messages SET status = 'error', error = $2, updated_at = now() WHERE id = $1;

-- name: ThreadBusy :one
-- Há resposta do assistente em andamento nesta conversa? (uma pergunta por vez)
SELECT EXISTS (
    SELECT 1 FROM chat_messages WHERE thread_id = $1 AND role = 'assistant' AND status IN ('pending', 'running')
)::boolean;

-- name: GameTrophiesForAI :many
-- Troféus do jogo (linha da conta) com o necessário para o contexto do modelo.
SELECT t.id, t.external_id, t.name, t.description, t.type, t.hidden, t.rarity_pct, t.earned,
       t.earned_at, t.progress, t.target, t.tags, t.notes, t.server_shutdown_risk,
       g.name AS group_name, g.is_base
FROM trophies t
JOIN trophy_groups g ON g.id = t.group_id
WHERE t.game_id = $1
ORDER BY g.is_base DESC, g.name, t.earned, t.rarity_pct DESC NULLS LAST, t.name;

-- name: GetTrophyWithGame :one
SELECT t.*, gm.user_id AS game_user_id, gm.platform AS game_platform, gm.external_id AS game_external_id, gm.title AS game_title,
       tg.name AS group_name, tg.is_base
FROM trophies t
JOIN games gm ON gm.id = t.game_id
JOIN trophy_groups tg ON tg.id = t.group_id
WHERE t.id = $1;

-- name: InsertAIRequest :exec
INSERT INTO ai_requests (user_id, kind) VALUES ($1, $2);

-- name: CountAIRequestsSince :one
SELECT count(*) FROM ai_requests WHERE user_id = $1 AND created_at >= $2;
