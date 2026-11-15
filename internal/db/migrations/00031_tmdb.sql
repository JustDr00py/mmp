-- +goose Up
-- TMDB lookups for the VOD library (Settings → VOD library). Searches are
-- cached per normalised title and year, misses too (tmdb_id ''), so one
-- title on many providers costs one search, and a refresh none.
CREATE TABLE tmdb_search (
    kind        TEXT NOT NULL CHECK (kind IN ('movie', 'series')),
    query_key   TEXT NOT NULL, -- vodmeta.Key of the title
    year        INT  NOT NULL, -- 0: no year
    tmdb_id     TEXT NOT NULL, -- '' when nothing matched
    searched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, query_key, year)
);

-- A matched title's TMDB details (the movie or tv answer with its
-- external ids), for metadata panels lack.
CREATE TABLE tmdb_cache (
    kind       TEXT NOT NULL CHECK (kind IN ('movie', 'series')),
    tmdb_id    TEXT NOT NULL,
    imdb_id    TEXT NOT NULL DEFAULT '',
    data       JSONB NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, tmdb_id)
);

-- +goose Down
DROP TABLE IF EXISTS tmdb_cache;
DROP TABLE IF EXISTS tmdb_search;
