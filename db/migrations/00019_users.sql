-- +goose Up
-- Multiusuário: cada pessoa vê só os próprios dados. O dono (owner) é criado aqui e herda TUDO o
-- que existe; ele "reivindica" a linha no login pelo e-mail verificado = OWNER_EMAIL (config).

CREATE TABLE users (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Identidade no IdP (iss + sub). NULL até o primeiro login (o dono nasce assim).
    issuer       text        NOT NULL DEFAULT '',
    subject      text,
    email        text        NOT NULL DEFAULT '',
    name         text        NOT NULL DEFAULT '',
    role         text        NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'member')),
    -- pending = logou mas ainda não aceitou convite; removed = acesso revogado pelo dono.
    status       text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active', 'removed')),
    invited_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    last_seen_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_users_identity ON users (issuer, subject) WHERE subject IS NOT NULL;
CREATE UNIQUE INDEX uq_users_single_owner ON users (role) WHERE role = 'owner';

-- Dono: id fixo (referência estável para o backfill e os testes).
INSERT INTO users (id, name, role, status) VALUES ('00000000-0000-0000-0000-000000000001', 'Dono', 'owner', 'active');

-- Contas (integrations) e jogos pertencem a um usuário. games.user_id é desnormalizado da conta:
-- quase toda leitura parte de games, e o escopo fica num índice só.
ALTER TABLE integrations ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;
UPDATE integrations SET user_id = '00000000-0000-0000-0000-000000000001';
ALTER TABLE integrations ALTER COLUMN user_id SET NOT NULL;
CREATE INDEX idx_integrations_user ON integrations (user_id);

ALTER TABLE games ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;
UPDATE games g SET user_id = i.user_id FROM integrations i WHERE i.id = g.integration_id;
ALTER TABLE games ALTER COLUMN user_id SET NOT NULL;
CREATE INDEX idx_games_user_key ON games (user_id, platform, external_id);

-- Push e chat: por usuário.
ALTER TABLE push_subscriptions ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;
UPDATE push_subscriptions SET user_id = '00000000-0000-0000-0000-000000000001';
ALTER TABLE push_subscriptions ALTER COLUMN user_id SET NOT NULL;
CREATE INDEX idx_push_subscriptions_user ON push_subscriptions (user_id);

ALTER TABLE chat_threads ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;
UPDATE chat_threads SET user_id = '00000000-0000-0000-0000-000000000001';
ALTER TABLE chat_threads ALTER COLUMN user_id SET NOT NULL;
DROP INDEX uq_chat_threads_active;
CREATE UNIQUE INDEX uq_chat_threads_active ON chat_threads (user_id, platform, game_external_id) WHERE NOT archived;

-- Guias: o de TROFÉU é compartilhado (user_id NULL — o mesmo guia serve a todos e economiza cota);
-- o ROADMAP é de cada usuário (depende do progresso dele). A edição do usuário vira privada.
ALTER TABLE ai_guides ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;
UPDATE ai_guides SET user_id = '00000000-0000-0000-0000-000000000001' WHERE scope = 'roadmap';
ALTER TABLE ai_guides ADD CONSTRAINT ai_guides_owner_by_scope CHECK ((scope = 'roadmap') = (user_id IS NOT NULL));
-- +goose StatementBegin
DO $$
DECLARE c text;
BEGIN
    FOR c IN SELECT conname FROM pg_constraint WHERE conrelid = 'ai_guides'::regclass AND contype = 'u' LOOP
        EXECUTE format('ALTER TABLE ai_guides DROP CONSTRAINT %I', c);
    END LOOP;
END $$;
-- +goose StatementEnd
CREATE UNIQUE INDEX uq_ai_guides_trophy ON ai_guides (platform, game_external_id, trophy_external_id) WHERE scope = 'trophy';
CREATE UNIQUE INDEX uq_ai_guides_roadmap ON ai_guides (user_id, platform, game_external_id) WHERE scope = 'roadmap';

CREATE TABLE ai_guide_edits (
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    guide_id   uuid        NOT NULL REFERENCES ai_guides (id) ON DELETE CASCADE,
    content    jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, guide_id)
);
INSERT INTO ai_guide_edits (user_id, guide_id, content)
SELECT '00000000-0000-0000-0000-000000000001', id, edited_content FROM ai_guides WHERE edited_content IS NOT NULL;
ALTER TABLE ai_guides DROP COLUMN edited_content;

-- Pedidos de IA por usuário (teto diário dos membros; o dono não tem teto).
CREATE TABLE ai_requests (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_ai_requests_user_time ON ai_requests (user_id, created_at DESC);

-- Convites: só o hash do código fica no banco (o link é mostrado uma vez ao dono).
CREATE TABLE invites (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    code_hash  text        NOT NULL UNIQUE,
    created_by uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    max_uses   integer     NOT NULL DEFAULT 1 CHECK (max_uses > 0),
    used_count integer     NOT NULL DEFAULT 0,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Preferências de notificação passam a ser por usuário (settings "notifications:<user_id>").
UPDATE settings SET key = 'notifications:00000000-0000-0000-0000-000000000001' WHERE key = 'notifications';

-- Views de progresso: expõem user_id e o "best" passa a ser POR USUÁRIO (contas de pessoas
-- diferentes nunca disputam o mesmo jogo).
DROP VIEW best_games;
DROP VIEW game_progress;
CREATE VIEW game_progress AS
SELECT g.id, g.user_id, g.integration_id, g.platform, g.external_id, g.title, g.cover, g.hero,
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
           ROW_NUMBER() OVER (PARTITION BY gp.user_id, gp.platform, gp.external_id
                              ORDER BY gp.completion_pct DESC, gp.earned DESC,
                                       gp.last_unlock_at DESC NULLS LAST, gp.id) AS account_rank
    FROM game_progress gp
) ranked
WHERE account_rank = 1;

-- +goose Down
DROP VIEW best_games;
DROP VIEW game_progress;
CREATE VIEW game_progress AS
SELECT g.id, g.integration_id, g.platform, g.external_id, g.title, g.cover, g.hero,
       g.has_achievements, g.abandoned, g.priority, g.tags, g.last_played_at,
       COUNT(t.id)::int AS total, (COUNT(t.id) FILTER (WHERE t.earned))::int AS earned,
       MAX(t.earned_at)::timestamptz AS last_unlock_at,
       (CASE WHEN COUNT(t.id) > 0 THEN (COUNT(t.id) FILTER (WHERE t.earned))::float8 / COUNT(t.id) ELSE 0 END)::float8 AS completion_pct
FROM games g LEFT JOIN trophies t ON t.game_id = g.id GROUP BY g.id;
CREATE VIEW best_games AS
SELECT * FROM (
    SELECT gp.*, ROW_NUMBER() OVER (PARTITION BY gp.platform, gp.external_id
        ORDER BY gp.completion_pct DESC, gp.earned DESC, gp.last_unlock_at DESC NULLS LAST, gp.id) AS account_rank
    FROM game_progress gp
) ranked WHERE account_rank = 1;
UPDATE settings SET key = 'notifications' WHERE key = 'notifications:00000000-0000-0000-0000-000000000001';
DROP TABLE invites;
DROP TABLE ai_requests;
ALTER TABLE ai_guides ADD COLUMN edited_content jsonb;
UPDATE ai_guides a SET edited_content = e.content FROM ai_guide_edits e WHERE e.guide_id = a.id;
DROP TABLE ai_guide_edits;
DROP INDEX uq_ai_guides_roadmap;
DROP INDEX uq_ai_guides_trophy;
DELETE FROM ai_guides a USING ai_guides b WHERE a.scope = 'roadmap' AND b.scope = 'roadmap'
    AND a.platform = b.platform AND a.game_external_id = b.game_external_id AND a.id > b.id;
ALTER TABLE ai_guides ADD CONSTRAINT ai_guides_scope_key UNIQUE (scope, platform, game_external_id, trophy_external_id);
ALTER TABLE ai_guides DROP CONSTRAINT ai_guides_owner_by_scope;
ALTER TABLE ai_guides DROP COLUMN user_id;
DROP INDEX uq_chat_threads_active;
ALTER TABLE chat_threads DROP COLUMN user_id;
CREATE UNIQUE INDEX uq_chat_threads_active ON chat_threads (platform, game_external_id) WHERE NOT archived;
ALTER TABLE push_subscriptions DROP COLUMN user_id;
ALTER TABLE games DROP COLUMN user_id;
ALTER TABLE integrations DROP COLUMN user_id;
DROP TABLE users;
