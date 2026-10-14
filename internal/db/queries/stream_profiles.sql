-- name: CreateStreamProfile :one
INSERT INTO stream_profiles (name, mode, ffmpeg_args, user_agent)
VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetStreamProfile :one
SELECT * FROM stream_profiles WHERE id = $1;

-- name: ListStreamProfiles :many
SELECT * FROM stream_profiles ORDER BY name;

-- name: UpdateStreamProfile :one
UPDATE stream_profiles SET name = $2, mode = $3, ffmpeg_args = $4, user_agent = $5
WHERE id = $1 RETURNING *;

-- name: DeleteStreamProfile :execrows
DELETE FROM stream_profiles WHERE id = $1;

-- name: ProfileChannelCounts :many
SELECT stream_profile_id AS profile_id, count(*) AS channels
FROM channels WHERE stream_profile_id IS NOT NULL GROUP BY stream_profile_id;
