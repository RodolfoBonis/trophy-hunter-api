-- +goose Up
-- IA (Fase 4): guias por troféu, roadmap de platina e chat por jogo. Tudo chaveado pelo JOGO da
-- plataforma (platform + external_id), não pela linha da conta: o guia gerado numa conta Steam vale
-- para a outra e sobrevive a resyncs/remoção de conta.

-- ai_guides: um por troféu (scope trophy) ou por jogo (scope roadmap, trophy_external_id = '').
-- content = saída validada do modelo; edited_content = correção do usuário (vence na leitura).
CREATE TABLE ai_guides (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    scope               text        NOT NULL CHECK (scope IN ('trophy', 'roadmap')),
    platform            text        NOT NULL CHECK (platform IN ('steam', 'psn')),
    game_external_id    text        NOT NULL,
    trophy_external_id  text        NOT NULL DEFAULT '',
    status              text        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'ready', 'error')),
    content             jsonb,
    edited_content      jsonb,
    schema_version      integer     NOT NULL DEFAULT 1,
    model               text        NOT NULL DEFAULT '',
    error               text        NOT NULL DEFAULT '',
    generated_at        timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope, platform, game_external_id, trophy_external_id),
    CHECK ((scope = 'roadmap') = (trophy_external_id = ''))
);

-- chat_threads: uma conversa ativa por jogo; claude_session_id alimenta o --resume da CLI.
CREATE TABLE chat_threads (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    platform          text        NOT NULL CHECK (platform IN ('steam', 'psn')),
    game_external_id  text        NOT NULL,
    claude_session_id text        NOT NULL DEFAULT '',
    archived          boolean     NOT NULL DEFAULT false,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_chat_threads_active ON chat_threads (platform, game_external_id) WHERE NOT archived;

CREATE TABLE chat_messages (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    thread_id  uuid        NOT NULL REFERENCES chat_threads (id) ON DELETE CASCADE,
    role       text        NOT NULL CHECK (role IN ('user', 'assistant')),
    content    text        NOT NULL DEFAULT '',
    status     text        NOT NULL DEFAULT 'done' CHECK (status IN ('pending', 'running', 'done', 'error')),
    error      text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_chat_messages_thread ON chat_messages (thread_id, created_at);

-- +goose Down
DROP TABLE chat_messages;
DROP TABLE chat_threads;
DROP TABLE ai_guides;
