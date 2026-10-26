-- +goose Up
-- The channels each guide lists (its <channel> ids and display names), so
-- the admin can look up which id a guide uses for a channel instead of
-- reading the XML. Replaced with the programs on every download.
CREATE TABLE epg_channels (
    epg_source_id BIGINT NOT NULL REFERENCES epg_sources(id) ON DELETE CASCADE,
    tvg_id        TEXT   NOT NULL,
    display_names TEXT[] NOT NULL DEFAULT '{}',
    program_count INT    NOT NULL DEFAULT 0,
    PRIMARY KEY (epg_source_id, tvg_id)
);

-- +goose Down
DROP TABLE IF EXISTS epg_channels;
