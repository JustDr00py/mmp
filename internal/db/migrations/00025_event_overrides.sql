-- +goose Up
-- Game routing: while a team's game is on, its channel plays the event
-- slot channel that carries the game ("NBA 06") first and its own streams
-- after. Rows are rewritten by each routing pass; an expired row is simply
-- ignored, so the channel goes back to its own list by itself.
CREATE TABLE channel_event_overrides (
    channel_id        BIGINT      PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
    source_channel_id BIGINT      NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    title             TEXT        NOT NULL DEFAULT '',
    starts_at         TIMESTAMPTZ NOT NULL,
    ends_at           TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (channel_id <> source_channel_id),
    CHECK (ends_at > starts_at)
);

-- +goose Down
DROP TABLE IF EXISTS channel_event_overrides;
