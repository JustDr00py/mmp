package channel

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sysadmin/mmp/internal/db"
)

// CompileRule compiles an admin rule pattern. Matching is case-insensitive.
func CompileRule(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile("(?i)" + pattern)
}

// Service runs matching passes against the database.
type Service struct {
	log  *slog.Logger
	pool *pgxpool.Pool
}

// NewService builds a Service.
func NewService(log *slog.Logger, pool *pgxpool.Pool) *Service {
	return &Service{log: log, pool: pool}
}

// Propose groups every unassigned live stream into proposals.
func (s *Service) Propose(ctx context.Context) ([]Proposal, error) {
	return s.propose(ctx, db.New(s.pool))
}

func (s *Service) propose(ctx context.Context, q *db.Queries) ([]Proposal, error) {
	rows, err := q.ListUnassignedStreams(ctx)
	if err != nil {
		return nil, fmt.Errorf("match: list streams: %w", err)
	}
	chans, err := q.ListChannelMatchKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("match: list channels: %w", err)
	}
	dbRules, err := q.ListMatchRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("match: list rules: %w", err)
	}

	streams := make([]Stream, len(rows))
	for i, r := range rows {
		streams[i] = Stream{
			ID: r.ID, ProviderID: r.ProviderID, ProviderPriority: r.ProviderPriority,
			Name: r.Name, TvgID: r.TvgID, LogoURL: r.LogoUrl,
		}
	}
	existing := make([]Existing, len(chans))
	for i, c := range chans {
		existing[i] = Existing{ID: c.ID, Name: c.Name, TvgID: c.TvgID}
	}
	rules := make([]Rule, 0, len(dbRules))
	for _, r := range dbRules {
		if !r.Enabled {
			continue
		}
		re, err := CompileRule(r.Pattern)
		if err != nil { // validated on create; skip rather than fail the pass
			s.log.Warn("match: skipping invalid rule", "rule_id", r.ID, "err", err)
			continue
		}
		rules = append(rules, Rule{ID: r.ID, Pattern: re, ChannelName: r.ChannelName})
	}
	return Match(streams, existing, rules), nil
}

// ApplyOptions selects which proposals Apply acts on.
type ApplyOptions struct {
	Keys         []string // only these proposal keys; empty = all
	MinProviders int      // skip groups seen at fewer distinct providers
}

// ApplyResult summarises an Apply.
type ApplyResult struct {
	Created  int `json:"created"`  // new channels
	Attached int `json:"attached"` // streams added to failover lists
}

// Apply recomputes proposals and commits the selected ones in a single
// transaction: new channels are created and every grouped stream is
// appended to its channel's failover list.
func (s *Service) Apply(ctx context.Context, opts ApplyOptions) (ApplyResult, error) {
	var res ApplyResult
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("match apply: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	q := db.New(tx)

	proposals, err := s.propose(ctx, q)
	if err != nil {
		return res, err
	}
	want := map[string]bool{}
	for _, k := range opts.Keys {
		want[k] = true
	}

	for _, p := range proposals {
		if len(want) > 0 && !want[p.Key] {
			continue
		}
		if p.Providers < opts.MinProviders {
			continue
		}
		chID := p.ChannelID
		if chID == 0 {
			num, err := q.NextChannelNumber(ctx)
			if err != nil {
				return res, fmt.Errorf("match apply %q: number: %w", p.Key, err)
			}
			c, err := q.CreateChannel(ctx, db.CreateChannelParams{
				Number: &num, Name: p.Name, TvgID: p.TvgID, LogoUrl: p.LogoURL,
			})
			if err != nil {
				return res, fmt.Errorf("match apply %q: create channel: %w", p.Key, err)
			}
			chID = c.ID
			res.Created++
		}
		base, err := q.MaxChannelStreamPriority(ctx, chID)
		if err != nil {
			return res, fmt.Errorf("match apply %q: priority: %w", p.Key, err)
		}
		for i, sid := range p.StreamIDs {
			if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{
				ChannelID: chID, ProviderStreamID: sid, Priority: base + int32(i) + 1,
			}); err != nil {
				return res, fmt.Errorf("match apply %q: add stream %d: %w", p.Key, sid, err)
			}
			res.Attached++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("match apply: commit: %w", err)
	}
	s.log.Info("match applied", "created", res.Created, "attached", res.Attached)
	return res, nil
}
