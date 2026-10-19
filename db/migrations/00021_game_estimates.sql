-- +goose Up
-- Estimativas de tempo (HowLongToBeat) por TÍTULO normalizado — valem para Steam e PSN e para todos
-- os usuários. Preenchidas em segundo plano pelo worker; ausência = sem estimativa (a UI se vira).
CREATE TABLE game_estimates (
    normalized_title text        PRIMARY KEY,
    status           text        NOT NULL CHECK (status IN ('found', 'not_found')),
    hltb_id          bigint,
    hltb_name        text,
    main_h           real,
    extra_h          real,
    completionist_h  real,
    fetched_at       timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE game_estimates;
