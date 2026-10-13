package proxy

import (
	"context"
	"fmt"

	"github.com/sysadmin/mmp/internal/db"
)

// CandidateQuerier is the query the proxy needs (satisfied by *db.Queries).
type CandidateQuerier interface {
	ListChannelCandidates(ctx context.Context, channelID int64) ([]db.ListChannelCandidatesRow, error)
}

// DBSource loads failover lists from Postgres for the manager.
func DBSource(q CandidateQuerier) ChannelSource {
	return func(ctx context.Context, channelID int64) ([]Candidate, error) {
		rows, err := q.ListChannelCandidates(ctx, channelID)
		if err != nil {
			return nil, fmt.Errorf("load candidates for channel %d: %w", channelID, err)
		}
		out := make([]Candidate, len(rows))
		for i, r := range rows {
			ua := r.UserAgent
			if r.ProfileUserAgent != "" { // the channel's profile is more specific
				ua = r.ProfileUserAgent
			}
			out[i] = Candidate{
				StreamID:       r.StreamID,
				ProviderID:     r.ProviderID,
				MaxConnections: int(r.MaxConnections),
				URL:            r.StreamUrl,
				UserAgent:      ua,
				FFmpeg:         r.ProfileMode == "ffmpeg",
				FFmpegArgs:     r.ProfileFfmpegArgs,
			}
		}
		return out, nil
	}
}
