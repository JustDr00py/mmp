-- +goose Up
-- Who did what in the admin UI and API: sign-ins (and failed ones), every
-- change, and backup downloads. Request bodies are never stored.
CREATE TABLE admin_audit (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts          TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_id     BIGINT REFERENCES users(id) ON DELETE SET NULL,
    username    TEXT        NOT NULL DEFAULT '',
    ip          TEXT        NOT NULL DEFAULT '',
    action      TEXT        NOT NULL,             -- e.g. "deleted", "signed in"
    target_type TEXT        NOT NULL DEFAULT '',  -- e.g. "provider"
    target_id   TEXT        NOT NULL DEFAULT '',
    target_name TEXT        NOT NULL DEFAULT '',
    method      TEXT        NOT NULL DEFAULT '',
    path        TEXT        NOT NULL DEFAULT '',
    via         TEXT        NOT NULL DEFAULT 'ui' -- "ui" or "api"
);
CREATE INDEX idx_admin_audit_ts ON admin_audit (ts DESC);
CREATE INDEX idx_admin_audit_user ON admin_audit (username, ts DESC);

-- +goose Down
DROP TABLE IF EXISTS admin_audit;
