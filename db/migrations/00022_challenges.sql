-- +goose Up
-- Desafios entre amigos (exigem compartilhamento ligado, como o resto do social).
--   first_to_complete: quem fechar o jogo (platina/100%) primeiro — mesmo jogo por plataforma+id OU
--                      título normalizado (Steam × PSN); prazo opcional;
--   most_trophies / most_rare: mais troféus (ou raros, < 5%) obtidos entre starts_at e ends_at.
CREATE TABLE challenges (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    creator_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind             text        NOT NULL CHECK (kind IN ('first_to_complete', 'most_trophies', 'most_rare')),
    title            text        NOT NULL,
    game_platform    text,
    game_external_id text,
    normalized_title text,
    game_title       text,
    game_cover       text,
    starts_at        timestamptz NOT NULL DEFAULT now(),
    ends_at          timestamptz,
    status           text        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'finished', 'canceled')),
    winner_id        uuid        REFERENCES users (id) ON DELETE SET NULL,
    finished_at      timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CHECK (kind <> 'first_to_complete' OR (game_platform IS NOT NULL AND game_external_id IS NOT NULL AND normalized_title IS NOT NULL)),
    CHECK (kind = 'first_to_complete' OR ends_at IS NOT NULL)
);
CREATE INDEX idx_challenges_open ON challenges (status) WHERE status = 'open';

CREATE TABLE challenge_participants (
    challenge_id   uuid        NOT NULL REFERENCES challenges (id) ON DELETE CASCADE,
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    state          text        NOT NULL DEFAULT 'invited' CHECK (state IN ('invited', 'accepted', 'declined')),
    -- first_to_complete: quando fechou o jogo; last_milestone: último marco avisado (50/75/90 %).
    completed_at   timestamptz,
    last_milestone smallint    NOT NULL DEFAULT 0,
    responded_at   timestamptz,
    PRIMARY KEY (challenge_id, user_id)
);
CREATE INDEX idx_challenge_participants_user ON challenge_participants (user_id);

-- +goose Down
DROP TABLE challenge_participants;
DROP TABLE challenges;
