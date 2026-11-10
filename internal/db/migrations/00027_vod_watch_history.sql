-- +goose Up
-- Movie and episode playback in the watch history. A VOD row has
-- channel_id 0 and output 'movie' or 'series'; item_id is the movie or
-- episode, title its name when played (history outlives the catalog).
-- bytes is what reached the player; end_reason is why the session ended
-- ('stopped', 'kicked', 'taken over', 'upstream error'; '' for live rows
-- written before this column).
ALTER TABLE viewing_sessions
    ADD COLUMN item_id    BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN title      TEXT   NOT NULL DEFAULT '',
    ADD COLUMN bytes      BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN end_reason TEXT   NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE viewing_sessions
    DROP COLUMN IF EXISTS end_reason,
    DROP COLUMN IF EXISTS bytes,
    DROP COLUMN IF EXISTS title,
    DROP COLUMN IF EXISTS item_id;
