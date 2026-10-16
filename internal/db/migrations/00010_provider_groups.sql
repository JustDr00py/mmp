-- +goose Up
-- Per-provider group selection. With group_filter on, only streams whose
-- group is listed in provider_groups are offered for matching and in the
-- stream picker; groups a provider adds later stay hidden until selected.
-- Streams already on channels keep playing either way.
ALTER TABLE providers ADD COLUMN group_filter BOOLEAN NOT NULL DEFAULT FALSE;
CREATE TABLE provider_groups (
    provider_id BIGINT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    group_title TEXT   NOT NULL,
    PRIMARY KEY (provider_id, group_title)
);

-- +goose Down
DROP TABLE IF EXISTS provider_groups;
ALTER TABLE providers DROP COLUMN IF EXISTS group_filter;
