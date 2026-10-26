-- Background prober.

-- name: ListProbeTargets :many
-- Streams that can actually be played: on an enabled channel, at an enabled
-- provider, not soft-disabled. A channel profile's user agent wins over the
-- provider's, as it does for playback.
SELECT ps.id AS stream_id, ps.stream_url, ps.health_score, ps.cooldown_until, ps.last_probed_at,
       pr.id AS provider_id, pr.max_connections, pr.backbone,
       COALESCE(NULLIF(max(sp.user_agent), ''), pr.user_agent)::text AS user_agent
FROM channel_streams cs
JOIN channels c ON c.id = cs.channel_id AND c.enabled
JOIN provider_streams ps ON ps.id = cs.provider_stream_id AND NOT ps.disabled
JOIN providers pr ON pr.id = ps.provider_id AND pr.enabled
LEFT JOIN stream_profiles sp ON sp.id = c.stream_profile_id
GROUP BY ps.id, pr.id
ORDER BY ps.id;

-- name: RecordProbe :exec
UPDATE provider_streams
SET health_score = $2, last_error = $3, cooldown_until = $4, last_probed_at = $5
WHERE id = $1;
