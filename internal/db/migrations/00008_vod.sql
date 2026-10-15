-- +goose Up
-- VOD (movies) and series catalogs imported from Xtream providers, for
-- pass-through playback. Rows are keyed per provider by the panel's ids and
-- soft-disabled (not deleted) when they disappear, like provider_streams.

ALTER TABLE providers
    ADD COLUMN vod_enabled      BOOLEAN NOT NULL DEFAULT FALSE, -- import movies/series (catalogs are large)
    ADD COLUMN vod_refreshed_at TIMESTAMPTZ;

-- Movie and series categories, as the panel names them.
CREATE TABLE vod_categories (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_id BIGINT  NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL CHECK (kind IN ('movie', 'series')),
    external_id TEXT    NOT NULL,
    name        TEXT    NOT NULL,
    disabled    BOOLEAN NOT NULL DEFAULT FALSE,
    last_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, kind, external_id)
);

CREATE TABLE vod_items (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_id          BIGINT  NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    external_id          TEXT    NOT NULL, -- the panel's stream_id
    name                 TEXT    NOT NULL,
    category_external_id TEXT    NOT NULL DEFAULT '',
    icon                 TEXT    NOT NULL DEFAULT '',
    rating               REAL,             -- 0-10 as the panel reports it
    container_extension  TEXT    NOT NULL DEFAULT 'mp4',
    tmdb_id              TEXT    NOT NULL DEFAULT '',
    added_at             TIMESTAMPTZ,
    info                 JSONB,            -- get_vod_info answer, cached on demand
    info_fetched_at      TIMESTAMPTZ,
    disabled             BOOLEAN NOT NULL DEFAULT FALSE,
    last_seen            TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, external_id)
);
CREATE INDEX idx_vod_items_category ON vod_items (provider_id, category_external_id) WHERE NOT disabled;

CREATE TABLE series (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_id          BIGINT  NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    external_id          TEXT    NOT NULL, -- the panel's series_id
    name                 TEXT    NOT NULL,
    category_external_id TEXT    NOT NULL DEFAULT '',
    cover                TEXT    NOT NULL DEFAULT '',
    plot                 TEXT    NOT NULL DEFAULT '',
    "cast"               TEXT    NOT NULL DEFAULT '',
    director             TEXT    NOT NULL DEFAULT '',
    genre                TEXT    NOT NULL DEFAULT '',
    release_date         TEXT    NOT NULL DEFAULT '',
    rating               REAL,
    last_modified        TIMESTAMPTZ,
    info                 JSONB,            -- get_series_info "info" block, cached with the episodes
    episodes_fetched_at  TIMESTAMPTZ,      -- episodes are loaded lazily, per series
    disabled             BOOLEAN NOT NULL DEFAULT FALSE,
    last_seen            TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, external_id)
);
CREATE INDEX idx_series_category ON series (provider_id, category_external_id) WHERE NOT disabled;

CREATE TABLE series_episodes (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    series_id           BIGINT  NOT NULL REFERENCES series(id) ON DELETE CASCADE,
    external_id         TEXT    NOT NULL, -- the panel's episode id
    season              INT     NOT NULL DEFAULT 0,
    episode_num         INT     NOT NULL DEFAULT 0,
    title               TEXT    NOT NULL DEFAULT '',
    container_extension TEXT    NOT NULL DEFAULT 'mp4',
    plot                TEXT    NOT NULL DEFAULT '',
    duration_secs       INT     NOT NULL DEFAULT 0,
    image               TEXT    NOT NULL DEFAULT '',
    added_at            TIMESTAMPTZ,
    UNIQUE (series_id, external_id)
);
CREATE INDEX idx_series_episodes_order ON series_episodes (series_id, season, episode_num);

-- +goose Down
DROP TABLE IF EXISTS series_episodes;
DROP TABLE IF EXISTS series;
DROP TABLE IF EXISTS vod_items;
DROP TABLE IF EXISTS vod_categories;
ALTER TABLE providers DROP COLUMN IF EXISTS vod_refreshed_at;
ALTER TABLE providers DROP COLUMN IF EXISTS vod_enabled;
