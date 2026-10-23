package channel

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

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

	// The last Propose, reused for proposeTTL so paging and searching the
	// Match page do not rematch every stream. Invalidate drops it.
	cacheMu  sync.Mutex
	cached   []Proposal
	cachedAt time.Time
}

// proposeTTL bounds how stale a cached Propose can be after a change that
// does not call Invalidate.
const proposeTTL = time.Minute

// NewService builds a Service.
func NewService(log *slog.Logger, pool *pgxpool.Pool) *Service {
	return &Service{log: log, pool: pool}
}

// Propose groups every unassigned live stream into proposals. The result
// is cached (see Invalidate) and shared: callers must not modify it.
func (s *Service) Propose(ctx context.Context) ([]Proposal, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cached != nil && time.Since(s.cachedAt) < proposeTTL {
		return s.cached, nil
	}
	props, err := s.propose(ctx, db.New(s.pool))
	if err != nil {
		return nil, err
	}
	s.cached, s.cachedAt = props, time.Now()
	return props, nil
}

// Invalidate drops the cached proposals. Call it after changing what
// matching reads: streams, channels, rules, merges or ignored prefixes.
func (s *Service) Invalidate() {
	s.cacheMu.Lock()
	s.cached = nil
	s.cacheMu.Unlock()
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
	prefixes, err := LoadIgnorePrefixes(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("match: %w", err)
	}
	merges, err := LoadMerges(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("match: %w", err)
	}

	streams := make([]Stream, 0, len(rows))
	for _, r := range rows {
		if IsPlaceholder(r.Name) { // section headers, not channels
			continue
		}
		streams = append(streams, Stream{
			ID: r.ID, ProviderID: r.ProviderID, ProviderPriority: r.ProviderPriority,
			Name: r.Name, MatchName: StripPrefixes(r.Name, prefixes), TvgID: r.TvgID, LogoURL: r.LogoUrl, Group: r.GroupTitle,
			Backbone: r.Backbone,
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
	return Match(streams, existing, rules, merges), nil
}

// ApplyOptions selects which proposals Apply acts on.
type ApplyOptions struct {
	Keys         []string // only these proposal keys; empty = all
	MinProviders int      // skip groups seen at fewer sources (Proposal.Providers)
	// AttachAlways exempts proposals for existing channels from
	// MinProviders, so a new provider's streams join channels that exist.
	AttachAlways bool

	// Names overrides the name of a new channel, by proposal key.
	Names map[string]string
	// Streams limits a proposal (by key) to these stream ids; streams not
	// listed stay unassigned. A key mapped to no ids skips the proposal.
	Streams map[string][]int64
	// New channels go into ChannelGroupID, or the group named
	// ChannelGroupName, or with GroupFromSource into a channel group named
	// after the proposal's provider group (named groups are created when
	// missing).
	ChannelGroupID   *int64
	ChannelGroupName string
	GroupFromSource  bool
	// BouquetIDs each receive every channel the apply touched.
	BouquetIDs []int64
	// NewChannelBouquetIDs each receive only the channels this apply
	// created; channels that just gained streams keep their bouquets.
	NewChannelBouquetIDs []int64
	// NewChannelBouquetsFromGroup puts each created channel in the
	// bouquets that hold most (over half) of the other channels in its
	// group. A channel without a group, or in a group with no other
	// channels, joins none.
	NewChannelBouquetsFromGroup bool
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

	groups := map[string]int64{} // group name -> channel group id
	var touched, created []int64
	createdIn := map[int64]int64{} // created channel -> its group
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
			groupName := strings.TrimSpace(opts.ChannelGroupName)
			if groupName == "" && opts.GroupFromSource {
				groupName = strings.TrimSpace(p.Group)
			}
			if groupID == nil && groupName != "" {
				g, ok := groups[groupName]
				if !ok {
					cg, err := q.EnsureChannelGroup(ctx, groupName)
					if err != nil {
						return res, fmt.Errorf("match apply %q: channel group: %w", p.Key, err)
					}
					g = cg.ID
					groups[groupName] = g
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
			created = append(created, chID)
			if groupID != nil {
				createdIn[chID] = *groupID
			}
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
	if opts.NewChannelBouquetsFromGroup && len(createdIn) > 0 {
		if err := bouquetsFromGroups(ctx, q, created, createdIn); err != nil {
			return res, err
		}
	}
	for _, bq := range opts.NewChannelBouquetIDs {
		if len(created) == 0 {
			break
		}
		if _, err := q.AddChannelsToBouquet(ctx, db.AddChannelsToBouquetParams{BouquetID: bq, ChannelIds: created}); err != nil {
			return res, fmt.Errorf("match apply: bouquet %d: %w", bq, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("match apply: commit: %w", err)
	}
	s.Invalidate()
	s.log.Info("match applied", "created", res.Created, "attached", res.Attached)
	return res, nil
}

// bouquetsFromGroups adds each created channel to the bouquets holding
// over half of the other channels in its group.
func bouquetsFromGroups(ctx context.Context, q *db.Queries, created []int64, createdIn map[int64]int64) error {
	var groupIDs []int64
	for _, g := range createdIn {
		if !slices.Contains(groupIDs, g) {
			groupIDs = append(groupIDs, g)
		}
	}
	shares, err := q.GroupBouquetShares(ctx, db.GroupBouquetSharesParams{Exclude: append([]int64{}, created...), GroupIds: groupIDs})
	if err != nil {
		return fmt.Errorf("match apply: group bouquets: %w", err)
	}
	byGroup := map[int64][]int64{} // group -> bouquets most of it is in
	for _, r := range shares {
		if r.InBouquet*2 > r.GroupSize {
			byGroup[r.GroupID] = append(byGroup[r.GroupID], r.BouquetID)
		}
	}
	add := map[int64][]int64{} // bouquet -> created channels
	for _, ch := range created {
		g, ok := createdIn[ch]
		if !ok {
			continue
		}
		for _, b := range byGroup[g] {
			add[b] = append(add[b], ch)
		}
	}
	for b, chans := range add {
		if _, err := q.AddChannelsToBouquet(ctx, db.AddChannelsToBouquetParams{BouquetID: b, ChannelIds: chans}); err != nil {
			return fmt.Errorf("match apply: bouquet %d: %w", b, err)
		}
	}
	return nil
}

// bouquetMode reads AutoMatch.Bouquets: BouquetsAuto, BouquetsNone or
// BouquetsFixed (names).
func bouquetMode(v []string) string {
	if len(v) == 0 {
		return BouquetsAuto
	}
	if len(v) == 1 {
		switch strings.ToLower(strings.TrimSpace(v[0])) {
		case "auto", "":
			return BouquetsAuto
		case "none":
			return BouquetsNone
		}
	}
	return BouquetsFixed
}

// bouquetIDs resolves bouquet names (case-insensitively), warning about
// names that match none.
func (s *Service) bouquetIDs(ctx context.Context, names []string) ([]int64, error) {
	if len(names) == 0 {
		return nil, nil
	}
	all, err := db.New(s.pool).ListBouquets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bouquets: %w", err)
	}
	var ids []int64
	for _, n := range names {
		n = strings.TrimSpace(n)
		i := slices.IndexFunc(all, func(b db.Bouquet) bool { return strings.EqualFold(b.Name, n) })
		if i < 0 {
			s.log.Warn("auto-match: no bouquet with this name", "bouquet", n)
			continue
		}
		if !slices.Contains(ids, all[i].ID) {
			ids = append(ids, all[i].ID)
		}
	}
	return ids, nil
}
