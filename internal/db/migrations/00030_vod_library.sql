-- +goose Up
-- VOD library: what each panel title says about itself (clean title, year,
-- quality, language, parsed at import) and its TMDB identity, so copies of
-- one title on any provider list once, like versions of one Plex item.
--
-- title_key names the title across providers: its TMDB id when known,
-- else its normalised title and year. Language is part of it (an English
-- and a French copy are different titles to a viewer) and so is 4K (kept
-- apart so players can choose it); HD and FHD copies merge. Rows without
-- a key (no TMDB id, no year) keep the per-pool identity they had.
--
-- match_source: where tmdb_id came from ('panel', 'tmdb' search, or an
-- admin's 'manual' fix). A locked match is never overwritten by imports.

ALTER TABLE series ADD COLUMN tmdb_id TEXT NOT NULL DEFAULT '';

ALTER TABLE vod_items
    ADD COLUMN title          TEXT NOT NULL DEFAULT '',
    ADD COLUMN title_norm     TEXT NOT NULL DEFAULT '',
    ADD COLUMN year           INT  NOT NULL DEFAULT 0,
    ADD COLUMN quality        TEXT NOT NULL DEFAULT '',
    ADD COLUMN lang           TEXT NOT NULL DEFAULT '',
    ADD COLUMN imdb_id        TEXT NOT NULL DEFAULT '',
    ADD COLUMN match_source   TEXT NOT NULL DEFAULT '',
    ADD COLUMN match_locked   BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN match_tried_at TIMESTAMPTZ,
    ADD COLUMN title_key      TEXT GENERATED ALWAYS AS (
        CASE
            WHEN tmdb_id <> '' THEN 't:' || tmdb_id || ':' || lang
                || CASE WHEN quality = '4k' THEN ':4k' ELSE '' END
            WHEN title_norm <> '' AND year > 0 THEN 'n:' || title_norm || ':' || year::text || ':' || lang
                || CASE WHEN quality = '4k' THEN ':4k' ELSE '' END
        END) STORED;

ALTER TABLE series
    ADD COLUMN title          TEXT NOT NULL DEFAULT '',
    ADD COLUMN title_norm     TEXT NOT NULL DEFAULT '',
    ADD COLUMN year           INT  NOT NULL DEFAULT 0,
    ADD COLUMN quality        TEXT NOT NULL DEFAULT '',
    ADD COLUMN lang           TEXT NOT NULL DEFAULT '',
    ADD COLUMN imdb_id        TEXT NOT NULL DEFAULT '',
    ADD COLUMN match_source   TEXT NOT NULL DEFAULT '',
    ADD COLUMN match_locked   BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN match_tried_at TIMESTAMPTZ,
    ADD COLUMN title_key      TEXT GENERATED ALWAYS AS (
        CASE
            WHEN tmdb_id <> '' THEN 't:' || tmdb_id || ':' || lang
                || CASE WHEN quality = '4k' THEN ':4k' ELSE '' END
            WHEN title_norm <> '' AND year > 0 THEN 'n:' || title_norm || ':' || year::text || ':' || lang
                || CASE WHEN quality = '4k' THEN ':4k' ELSE '' END
        END) STORED;

CREATE INDEX idx_vod_items_title_key ON vod_items (title_key) WHERE NOT disabled AND title_key IS NOT NULL;
CREATE INDEX idx_series_title_key ON series (title_key) WHERE NOT disabled AND title_key IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_series_title_key;
DROP INDEX IF EXISTS idx_vod_items_title_key;
ALTER TABLE series
    DROP COLUMN IF EXISTS title_key, DROP COLUMN IF EXISTS match_tried_at, DROP COLUMN IF EXISTS match_locked,
    DROP COLUMN IF EXISTS match_source, DROP COLUMN IF EXISTS imdb_id, DROP COLUMN IF EXISTS lang,
    DROP COLUMN IF EXISTS quality, DROP COLUMN IF EXISTS year, DROP COLUMN IF EXISTS title_norm,
    DROP COLUMN IF EXISTS title, DROP COLUMN IF EXISTS tmdb_id;
ALTER TABLE vod_items
    DROP COLUMN IF EXISTS title_key, DROP COLUMN IF EXISTS match_tried_at, DROP COLUMN IF EXISTS match_locked,
    DROP COLUMN IF EXISTS match_source, DROP COLUMN IF EXISTS imdb_id, DROP COLUMN IF EXISTS lang,
    DROP COLUMN IF EXISTS quality, DROP COLUMN IF EXISTS year, DROP COLUMN IF EXISTS title_norm,
    DROP COLUMN IF EXISTS title;
