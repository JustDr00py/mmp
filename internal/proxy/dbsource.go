package proxy

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"

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
				Health:         r.HealthScore,
			}
		}
		return out, nil
	}
}

// SortedByHealth reorders src's candidates by health_score, best first,
// while enabled() reports true (checked per load, so it can be toggled at
// runtime). Scores are compared rounded to 0.1 and the sort is stable, so
// the admin's priority order still decides between similarly healthy
// streams and small score jitter does not reshuffle the list.
func SortedByHealth(src ChannelSource, enabled func() bool) ChannelSource {
	return func(ctx context.Context, channelID int64) ([]Candidate, error) {
		cands, err := src(ctx, channelID)
		if err != nil || !enabled() {
			return cands, err
		}
		slices.SortStableFunc(cands, func(a, b Candidate) int {
			return cmp.Compare(healthTier(b.Health), healthTier(a.Health))
		})
		return cands, nil
	}
}

func healthTier(h float32) int { return int(math.Round(float64(h) * 10)) }
