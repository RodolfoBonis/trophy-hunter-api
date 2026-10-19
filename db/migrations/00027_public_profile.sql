-- +goose Up
-- Perfil público (opt-in, separado do "compartilhar com amigos"): vitrine enxuta acessível sem login
-- em /u/<slug>. Trocar o slug invalida o link antigo; desligar some na hora (404).
ALTER TABLE users
    ADD COLUMN public_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN public_slug    text,
    ADD COLUMN public_name    text NOT NULL DEFAULT '',
    ADD CONSTRAINT users_public_slug_format CHECK (public_slug IS NULL OR public_slug ~ '^[a-z0-9](?:[a-z0-9-]{1,28}[a-z0-9])$');
CREATE UNIQUE INDEX uq_users_public_slug ON users (public_slug) WHERE public_slug IS NOT NULL;

-- +goose Down
DROP INDEX uq_users_public_slug;
ALTER TABLE users DROP CONSTRAINT users_public_slug_format, DROP COLUMN public_name, DROP COLUMN public_slug, DROP COLUMN public_enabled;
