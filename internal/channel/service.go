package channel

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/db"
)

// CompileRule compiles an admin rule pattern. Matching is case-insensitive.
func CompileRule(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile("(?i)" + pattern)
}

// Service runs matching passes against the database.
type Service struct {
	log  *slog.Logger
	pool *pgxpool.Pool
	// applyMu serialises Apply: an auto-match after a scheduled refresh and
	// an admin apply must not both create the same channel.
	applyMu sync.Mutex
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

	streams := make([]Stream, 0, len(rows))
	for _, r := range rows {
		if IsPlaceholder(r.Name) { // section headers, not channels
			continue
		}
		streams = append(streams, Stream{
			ID: r.ID, ProviderID: r.ProviderID, ProviderPriority: r.ProviderPriority,
			Name: r.Name, TvgID: r.TvgID, LogoURL: r.LogoUrl, Group: r.GroupTitle,
		})
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
	// AttachAlways exempts proposals for existing channels from
	// MinProviders, so a new provider's streams join channels that exist.
	AttachAlways bool

	// Names overrides the name of a new channel, by proposal key.
	Names map[string]string
	// Streams limits a proposal (by key) to these stream ids; streams not
	// listed stay unassigned. A key mapped to no ids skips the proposal.
	Streams map[string][]int64
	// New channels go into ChannelGroupID, or with GroupFromSource into a
	// channel group named after the proposal's provider group (created
	// when missing).
	ChannelGroupID  *int64
	GroupFromSource bool
	// BouquetIDs each receive every channel the apply touched.
	BouquetIDs []int64
	// ExistingOnly attaches streams to channels that exist and never
	// creates one.
	ExistingOnly bool
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
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
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

	groups := map[string]int64{} // provider group title -> channel group (GroupFromSource)
	var touched []int64
	for _, p := range proposals {
		if len(want) > 0 && !want[p.Key] {
			continue
		}
		if p.Providers < opts.MinProviders && !(opts.AttachAlways && p.ChannelID != 0) {
			continue
		}
		if opts.ExistingOnly && p.ChannelID == 0 {
			continue
		}
		if only, ok := opts.Streams[p.Key]; ok {
			p.StreamIDs = slices.DeleteFunc(slices.Clone(p.StreamIDs), func(id int64) bool { return !slices.Contains(only, id) })
			if len(p.StreamIDs) == 0 {
				continue
			}
		}
		chID := p.ChannelID
		if chID == 0 {
			num, err := q.NextChannelNumber(ctx)
			if err != nil {
				return res, fmt.Errorf("match apply %q: number: %w", p.Key, err)
			}
			name := p.Name
			if n := strings.TrimSpace(opts.Names[p.Key]); n != "" {
				name = n
			}
			groupID := opts.ChannelGroupID
			if groupID == nil && opts.GroupFromSource && strings.TrimSpace(p.Group) != "" {
				g, ok := groups[p.Group]
				if !ok {
					cg, err := q.EnsureChannelGroup(ctx, strings.TrimSpace(p.Group))
					if err != nil {
						return res, fmt.Errorf("match apply %q: channel group: %w", p.Key, err)
					}
					g = cg.ID
					groups[p.Group] = g
				}
				groupID = &g
			}
			c, err := q.CreateChannel(ctx, db.CreateChannelParams{
				Number: &num, Name: name, TvgID: p.TvgID, LogoUrl: p.LogoURL, GroupID: groupID,
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
		touched = append(touched, chID)
	}
	for _, bq := range opts.BouquetIDs {
		if len(touched) == 0 {
			break
		}
		if _, err := q.AddChannelsToBouquet(ctx, db.AddChannelsToBouquetParams{BouquetID: bq, ChannelIds: touched}); err != nil {
			return res, fmt.Errorf("match apply: bouquet %d: %w", bq, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("match apply: commit: %w", err)
	}
	s.log.Info("match applied", "created", res.Created, "attached", res.Attached)
	return res, nil
}

// AutoMatchHook returns a source.Refresher AfterRefresh hook: after a
// provider refresh it attaches new streams to matching existing channels
// and, with newChannels, creates channels for groups seen at >=
// minProviders providers. Failures are logged; the refresh itself already
// succeeded.
func (s *Service) AutoMatchHook(minProviders int, newChannels bool) func(context.Context, db.Provider) {
	return func(ctx context.Context, p db.Provider) {
		res, err := s.Apply(ctx, ApplyOptions{MinProviders: minProviders, AttachAlways: true, ExistingOnly: !newChannels})
		if err != nil {
			s.log.Error("auto-match failed", "provider_id", p.ID, "err", err)
			return
		}
		s.log.Info("auto-match", "provider_id", p.ID, "created", res.Created, "attached", res.Attached)
	}
}
