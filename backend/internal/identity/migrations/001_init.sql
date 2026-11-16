CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE users (
    id            uuid PRIMARY KEY,
    email         citext NOT NULL,
    password_hash text NOT NULL,
    full_name     text NOT NULL CHECK (length(full_name) BETWEEN 1 AND 200),
    phone         text NOT NULL DEFAULT '',
    role          text NOT NULL CHECK (role IN ('customer', 'hotel_manager', 'admin')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    version       bigint NOT NULL DEFAULT 1,
    CONSTRAINT users_email_key UNIQUE (email)
);

CREATE INDEX users_role_idx ON users (role, created_at DESC);

-- Refresh-токены хранятся только как SHA-256: утечка дампа БД не даёт
-- готовых токенов. family_id связывает цепочку ротаций: повторное
-- предъявление уже заменённого токена означает кражу, и вся семья отзывается.
CREATE TABLE refresh_tokens (
    token_hash  bytea PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id   uuid NOT NULL,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz,
    replaced_by bytea
);

CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);
