-- +goose Up
-- Customer activity for the admin UI: what each user watched, the client
-- networks they used, and failed password attempts. Pruned to the event
-- history setting.

-- One row per live viewing session (a TS connection or an HLS lease),
-- written when it ends. channel_id is not a foreign key: history outlives
-- a deleted channel.
CREATE TABLE viewing_sessions (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id BIGINT      NOT NULL,
    ip         TEXT        NOT NULL DEFAULT '',
    output     TEXT        NOT NULL DEFAULT 'ts',
    started_at TIMESTAMPTZ NOT NULL,
    ended_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_viewing_sessions_user ON viewing_sessions (user_id, started_at DESC);
CREATE INDEX idx_viewing_sessions_ended ON viewing_sessions (ended_at);

-- Client networks (an IPv4 address or an IPv6 /64) a user authenticated
-- from, with the last address seen there. The newest last_seen is the
-- user's "last seen".
CREATE TABLE user_networks (
    user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    network    TEXT        NOT NULL,
    last_ip    TEXT        NOT NULL DEFAULT '',
    first_seen TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, network)
);
CREATE INDEX idx_user_networks_seen ON user_networks (last_seen);

-- Wrong passwords. user_id is NULL for usernames that do not exist.
CREATE TABLE auth_failures (
    id       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts       TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_id  BIGINT REFERENCES users(id) ON DELETE CASCADE,
    username TEXT        NOT NULL,
    ip       TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX idx_auth_failures_user ON auth_failures (user_id, ts DESC);
CREATE INDEX idx_auth_failures_ts ON auth_failures (ts);

-- +goose Down
DROP TABLE IF EXISTS auth_failures;
DROP TABLE IF EXISTS user_networks;
DROP TABLE IF EXISTS viewing_sessions;
