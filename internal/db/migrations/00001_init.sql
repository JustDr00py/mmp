-- +goose Up
-- Initial MMP schema: providers, streams, channels, matching, EPG, users, events.

-- Providers: upstream M3U playlists or Xtream panels.
CREATE TABLE providers (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                 TEXT        NOT NULL,
    type                 TEXT        NOT NULL CHECK (type IN ('m3u', 'xtream')),
    url                  TEXT        NOT NULL,
    username             TEXT        NOT NULL DEFAULT '',
    password             TEXT        NOT NULL DEFAULT '',
    epg_url              TEXT        NOT NULL DEFAULT '',
    max_connections      INT         NOT NULL DEFAULT 1 CHECK (max_connections >= 0),
    user_agent           TEXT        NOT NULL DEFAULT '',
    refresh_interval_secs INT        NOT NULL DEFAULT 21600 CHECK (refresh_interval_secs > 0),
    priority             INT         NOT NULL DEFAULT 0,
    enabled              BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Raw streams discovered at each provider. Upserted by (provider_id, external_id).
CREATE TABLE provider_streams (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_id        BIGINT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    external_id        TEXT   NOT NULL,
    name               TEXT   NOT NULL,
    tvg_id             TEXT   NOT NULL DEFAULT '',
    logo_url           TEXT   NOT NULL DEFAULT '',
    group_title        TEXT   NOT NULL DEFAULT '',
    stream_url         TEXT   NOT NULL,
    health_score       REAL   NOT NULL DEFAULT 1.0,
    last_error         TEXT   NOT NULL DEFAULT '',
    cooldown_until     TIMESTAMPTZ,
    last_seen          TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled           BOOLEAN NOT NULL DEFAULT FALSE, -- soft-disabled when absent from a refresh
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, external_id)
);

CREATE INDEX idx_provider_streams_provider ON provider_streams(provider_id);
CREATE INDEX idx_provider_streams_tvg_id   ON provider_streams(tvg_id) WHERE tvg_id <> '';
CREATE INDEX idx_provider_streams_group    ON provider_streams(provider_id, group_title);

-- Optional per-channel transcoding/remux behaviour.
CREATE TABLE stream_profiles (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name         TEXT    NOT NULL UNIQUE,
    mode         TEXT    NOT NULL DEFAULT 'passthrough' CHECK (mode IN ('passthrough', 'ffmpeg')),
    ffmpeg_args  TEXT    NOT NULL DEFAULT '',
    user_agent   TEXT    NOT NULL DEFAULT ''
);

INSERT INTO stream_profiles (name, mode) VALUES ('default', 'passthrough');

-- Logical channel groups shown to customers.
CREATE TABLE channel_groups (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    sort_order INT     NOT NULL DEFAULT 0
);

-- Logical channels: what customers watch. One channel fans out to an
-- ordered failover list of provider streams.
CREATE TABLE channels (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    number             INT     UNIQUE,
    name               TEXT    NOT NULL,
    tvg_id             TEXT    NOT NULL DEFAULT '', -- EPG mapping (XMLTV channel id)
    logo_url           TEXT    NOT NULL DEFAULT '',
    group_id           BIGINT  REFERENCES channel_groups(id) ON DELETE SET NULL,
    stream_profile_id  BIGINT  REFERENCES stream_profiles(id) ON DELETE SET NULL,
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_channels_tvg_id ON channels(tvg_id) WHERE tvg_id <> '';
CREATE INDEX idx_channels_group  ON channels(group_id);

-- Ordered failover list: lower priority is preferred.
CREATE TABLE channel_streams (
    channel_id         BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    provider_stream_id BIGINT NOT NULL REFERENCES provider_streams(id) ON DELETE CASCADE,
    priority           INT    NOT NULL DEFAULT 0,
    added_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_id, provider_stream_id)
);

CREATE INDEX idx_channel_streams_stream ON channel_streams(provider_stream_id);

-- Bouquets: customer-facing channel collections.
CREATE TABLE bouquets (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    sort_order INT     NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE bouquet_channels (
    bouquet_id BIGINT NOT NULL REFERENCES bouquets(id)  ON DELETE CASCADE,
    channel_id BIGINT NOT NULL REFERENCES channels(id)  ON DELETE CASCADE,
    PRIMARY KEY (bouquet_id, channel_id)
);

-- EPG ingestion.
CREATE TABLE epg_sources (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name              TEXT        NOT NULL,
    url               TEXT        NOT NULL,
    provider_id       BIGINT      REFERENCES providers(id) ON DELETE CASCADE,
    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    last_refreshed_at TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE epg_programmes (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    epg_source_id  BIGINT NOT NULL REFERENCES epg_sources(id) ON DELETE CASCADE,
    channel_tvg_id TEXT   NOT NULL, -- <channel> id in the XMLTV file
    title          TEXT   NOT NULL,
    sub_title      TEXT   NOT NULL DEFAULT '',
    description    TEXT   NOT NULL DEFAULT '',
    start_time     TIMESTAMPTZ NOT NULL,
    stop_time      TIMESTAMPTZ NOT NULL,
    CHECK (stop_time > start_time),
    UNIQUE (epg_source_id, channel_tvg_id, start_time)
);

CREATE INDEX idx_epg_programmes_channel_time ON epg_programmes(channel_tvg_id, start_time DESC);

-- Users: admins and customers. Customer passwords are bcrypt hashes;
-- stream_token authenticates /playlist/{token}.m3u and /epg/{token}.xml.
CREATE TABLE users (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username        TEXT        NOT NULL UNIQUE,
    password_hash   TEXT        NOT NULL,
    role            TEXT        NOT NULL DEFAULT 'customer' CHECK (role IN ('admin', 'customer')),
    max_connections INT         NOT NULL DEFAULT 1 CHECK (max_connections >= 0),
    expires_at      TIMESTAMPTZ,
    enabled         BOOLEAN     NOT NULL DEFAULT TRUE,
    stream_token    TEXT        NOT NULL UNIQUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_bouquets (
    user_id    BIGINT NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    bouquet_id BIGINT NOT NULL REFERENCES bouquets(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, bouquet_id)
);

-- Failover and audit log.
CREATE TABLE events (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL DEFAULT now(),
    type           TEXT    NOT NULL, -- e.g. 'failover', 'session_close', 'audit'
    channel_id     BIGINT,
    provider_id    BIGINT,
    from_stream_id BIGINT,
    to_stream_id   BIGINT,
    message        TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_events_ts ON events(ts DESC);
CREATE INDEX idx_events_channel ON events(channel_id) WHERE channel_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS user_bouquets;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS epg_programmes;
DROP TABLE IF EXISTS epg_sources;
DROP TABLE IF EXISTS bouquet_channels;
DROP TABLE IF EXISTS bouquets;
DROP TABLE IF EXISTS channel_streams;
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS channel_groups;
DROP TABLE IF EXISTS stream_profiles;
DROP TABLE IF EXISTS provider_streams;
DROP TABLE IF EXISTS providers;
