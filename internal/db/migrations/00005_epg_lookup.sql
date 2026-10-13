-- +goose Up
-- Guide lookups match channel tvg-ids case-insensitively (the matcher
-- already treats tvg-ids that way), so index the folded key instead.
DROP INDEX IF EXISTS idx_epg_programmes_channel_time;
CREATE INDEX idx_epg_programmes_key_time ON epg_programmes (lower(channel_tvg_id), start_time);

-- +goose Down
DROP INDEX IF EXISTS idx_epg_programmes_key_time;
CREATE INDEX idx_epg_programmes_channel_time ON epg_programmes(channel_tvg_id, start_time DESC);
