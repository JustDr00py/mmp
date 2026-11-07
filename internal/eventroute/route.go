package eventroute

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/nameguide"
)

// Slot is an event slot channel with its streams' current names, in
// failover order.
type Slot struct {
	ID      int64
	Name    string
	Streams []string
}

// Team is a team channel.
type Team struct {
	ID   int64
	Name string
}

// Override says team channel ChannelID plays slot SourceID from Start to
// End.
type Override struct {
	ChannelID, SourceID            int64
	ChannelName, SourceName, Title string
	Start, End                     time.Time
}

// game is what a slot carries now by its streams' names.
type game struct {
	title string
	start time.Time
	teams []int64 // team channel ids, sorted
	votes int
	first int // position of the first stream naming it
}

// Plan works out the overrides at now: for each slot the game most of its
// streams name (providers disagree: one may label NBA 01 with another
// slot's game), when it starts within [now-after, now+before], and the
// team channels it names. A team named by two slots gets the one more
// streams agree on. loc is the zone of times a name gives without one.
func Plan(slots []Slot, teams []Team, now time.Time, loc *time.Location, before, after time.Duration) []Override {
	type teamKey struct {
		id   int64
		keys keys
	}
	names := map[int64]string{}
	for _, t := range teams {
		names[t.ID] = t.Name
	}
	var tk []teamKey
	for _, t := range teams {
		if k := teamKeys(t.Name); k != nil {
			tk = append(tk, teamKey{t.ID, k})
		}
	}
	best := map[int64]struct {
		o     Override
		votes int
	}{}
	for _, s := range slots {
		var games []*game
		for i, name := range s.Streams {
			ev := nameguide.Parse(name, now, loc)
			if ev.Start == nil || ev.Title == "" || !isMatchup(ev.Title) {
				continue
			}
			start := *ev.Start
			if now.Before(start.Add(-before)) || !now.Before(start.Add(after)) {
				continue // not on yet, or a name left over from an old game
			}
			title := norm(ev.Title)
			var ids []int64
			for _, t := range tk {
				if t.keys.in(title) {
					ids = append(ids, t.id)
				}
			}
			if len(ids) == 0 {
				continue
			}
			slices.Sort(ids)
			found := false
			for _, g := range games {
				if slices.Equal(g.teams, ids) {
					g.votes++
					found = true
					break
				}
			}
			if !found {
				games = append(games, &game{title: tidy(ev.Title), start: start, teams: ids, votes: 1, first: i})
			}
		}
		if len(games) == 0 {
			continue
		}
		sort.SliceStable(games, func(a, b int) bool { return games[a].votes > games[b].votes })
		g := games[0]
		for _, id := range g.teams {
			if cur, ok := best[id]; ok && (cur.votes > g.votes || (cur.votes == g.votes && cur.o.SourceID < s.ID)) {
				continue
			}
			best[id] = struct {
				o     Override
				votes int
			}{Override{ChannelID: id, ChannelName: names[id], SourceID: s.ID, SourceName: s.Name, Title: g.title,
				Start: g.start.Add(-before), End: g.start.Add(after)}, g.votes}
		}
	}
	out := make([]Override, 0, len(best))
	for _, b := range best {
		out = append(out, b.o)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ChannelID < out[b].ChannelID })
	return out
}

// tidy keeps a title's case unless it is all capitals.
func tidy(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s != strings.ToUpper(s) {
		return s
	}
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		rs := []rune(w)
		words[i] = strings.ToUpper(string(rs[0])) + string(rs[1:])
	}
	return strings.Join(words, " ")
}

// Router runs routing passes against the database.
type Router struct {
	log  *slog.Logger
	pool *pgxpool.Pool
	mu   sync.Mutex // the ticker and a refresh hook must not run passes at once
}

// NewRouter builds a Router.
func NewRouter(log *slog.Logger, pool *pgxpool.Pool) *Router {
	return &Router{log: log, pool: pool}
}

// Result says what a pass did.
type Result struct {
	Ran       bool       // false when routing is off
	Overrides []Override // in effect or coming up within the window
	Unlinked  int64      // slot streams taken off team channels
	Dropped   int64      // overrides no longer found
}

// Run makes one pass at now: it reads the settings, finds the games and
// rewrites channel_event_overrides. With routing off it clears them.
func (r *Router) Run(ctx context.Context, now time.Time) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var res Result
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		s, err := Load(ctx, q)
		if err != nil {
			return err
		}
		keep := []int64{}
		if s.Enabled {
			res.Ran = true
			ng, err := nameguide.Load(ctx, q)
			if err != nil {
				return err
			}
			for _, rule := range s.Rules {
				n, err := q.RemoveSlotLinksFromGroups(ctx, db.RemoveSlotLinksFromGroupsParams{
					TargetGroupIds: rule.TargetGroupIDs, SourceGroupIds: rule.SourceGroupIDs})
				if err != nil {
					return err
				}
				res.Unlinked += n
				slots, err := loadSlots(ctx, q, rule.SourceGroupIDs)
				if err != nil {
					return err
				}
				teams, err := loadTeams(ctx, q, rule.TargetGroupIDs)
				if err != nil {
					return err
				}
				for _, o := range Plan(slots, teams, now, ng.Location(), s.Before(), s.After()) {
					if slices.Contains(keep, o.ChannelID) {
						continue // an earlier rule routed this team
					}
					if err := q.UpsertEventOverride(ctx, db.UpsertEventOverrideParams{ChannelID: o.ChannelID,
						SourceChannelID: o.SourceID, Title: o.Title, StartsAt: o.Start, EndsAt: o.End}); err != nil {
						return err
					}
					keep = append(keep, o.ChannelID)
					res.Overrides = append(res.Overrides, o)
				}
			}
		}
		res.Dropped, err = q.DeleteEventOverridesExcept(ctx, keep)
		return err
	})
	if err != nil {
		return Result{}, fmt.Errorf("game routing: %w", err)
	}
	return res, nil
}

func loadSlots(ctx context.Context, q *db.Queries, groups []int64) ([]Slot, error) {
	rows, err := q.ListGroupChannelStreams(ctx, groups)
	if err != nil {
		return nil, err
	}
	var out []Slot
	for _, row := range rows {
		if len(out) == 0 || out[len(out)-1].ID != row.ChannelID {
			out = append(out, Slot{ID: row.ChannelID, Name: row.ChannelName})
		}
		if row.StreamName != nil {
			out[len(out)-1].Streams = append(out[len(out)-1].Streams, *row.StreamName)
		}
	}
	return out, nil
}

func loadTeams(ctx context.Context, q *db.Queries, groups []int64) ([]Team, error) {
	rows, err := q.ListGroupChannelStreams(ctx, groups)
	if err != nil {
		return nil, err
	}
	var out []Team
	for _, row := range rows {
		if len(out) == 0 || out[len(out)-1].ID != row.ChannelID {
			out = append(out, Team{ID: row.ChannelID, Name: row.ChannelName})
		}
	}
	return out, nil
}

// Every is how often the ticker runs a pass: games start on the hour and
// half hour, and slot names change through the day.
const Every = 5 * time.Minute

// Start runs a pass now and then every Every until ctx ends, logging what
// changes.
func (r *Router) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(Every)
		defer t.Stop()
		var last string
		for {
			res, err := r.Run(ctx, time.Now())
			switch {
			case err != nil && ctx.Err() == nil:
				r.log.Error("game routing failed", "err", err)
			case err == nil:
				last = r.logChanges(res, last)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// AfterRefresh is a source.Refresher hook: slot names change with a
// refresh, so route again.
func (r *Router) AfterRefresh(ctx context.Context, _ db.Provider) {
	if _, err := r.Run(ctx, time.Now()); err != nil {
		r.log.Error("game routing failed", "err", err)
	}
}

// logChanges logs a pass's routes when they differ from the last pass's
// (summary last), and returns the new summary.
func (r *Router) logChanges(res Result, last string) string {
	var b strings.Builder
	for _, o := range res.Overrides {
		fmt.Fprintf(&b, "%d>%d:%s;", o.ChannelID, o.SourceID, o.Title)
	}
	if b.String() != last || res.Unlinked > 0 {
		for _, o := range res.Overrides {
			r.log.Info("game routing", "channel_id", o.ChannelID, "source_channel_id", o.SourceID,
				"source", o.SourceName, "title", o.Title, "from", o.Start, "until", o.End)
		}
		if res.Unlinked > 0 || res.Dropped > 0 {
			r.log.Info("game routing changes", "slot_streams_unlinked", res.Unlinked, "routes_dropped", res.Dropped)
		}
	}
	return b.String()
}
