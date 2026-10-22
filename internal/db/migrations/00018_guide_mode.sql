-- +goose Up
-- Where a channel's guide comes from. 'auto': its XMLTV guide, else the
-- stream-name guide; 'xmltv': its XMLTV guide only; 'name': the
-- stream-name guide, else its XMLTV guide (event channels whose XMLTV
-- entries are placeholders). A channel's '' follows its group.
ALTER TABLE channel_groups
    ADD COLUMN guide_mode TEXT NOT NULL DEFAULT 'auto' CHECK (guide_mode IN ('auto', 'xmltv', 'name'));
ALTER TABLE channels
    ADD COLUMN guide_mode TEXT NOT NULL DEFAULT '' CHECK (guide_mode IN ('', 'auto', 'xmltv', 'name'));

-- +goose Down
ALTER TABLE channels DROP COLUMN IF EXISTS guide_mode;
ALTER TABLE channel_groups DROP COLUMN IF EXISTS guide_mode;
