-- +goose Up
-- Resumo semanal da IA (opt-in nas preferências): um por pessoa e semana (segunda-feira, horário de
-- Brasília). A retrospectiva mensal/anual é calculada na hora (sem tabela).
CREATE TABLE ai_recaps (
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    week       date        NOT NULL,
    status     text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ready', 'error')),
    content    jsonb,
    error      text,
    model      text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, week)
);

-- +goose Down
DROP TABLE ai_recaps;
