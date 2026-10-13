-- +goose Up
-- Admin authentication: browser sessions and named API tokens. Only the
-- SHA-256 of each secret is stored, never the secret itself.

CREATE TABLE admin_sessions (
    token_hash BYTEA       PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_admin_sessions_user    ON admin_sessions(user_id);
CREATE INDEX idx_admin_sessions_expires ON admin_sessions(expires_at);

CREATE TABLE api_tokens (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    token_hash   BYTEA       NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);

CREATE INDEX idx_api_tokens_user ON api_tokens(user_id);

-- +goose Down
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS admin_sessions;
