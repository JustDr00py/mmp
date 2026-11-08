//go:build integration

package db

import (
	"context"
	"slices"
	"testing"
	"time"
)

// TestEventOverrides: while a game override is on, a team channel's
// failover list is the slot's streams, then its own, each stream once; the
// slot streams added to it by hand are taken off; an expired or dropped
// override leaves its own list.
func TestEventOverrides(t *testing.T) {
	dsn := testDSN(t)
	recreate(t, dsn)
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := Open(ctx, PoolConfig{DSN: dsn})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	q := New(pool)

	p, err := q.CreateProvider(ctx, CreateProviderParams{Name: "p", Type: "m3u", Url: "http://p/",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	group := func(name string) int64 {
		g, err := q.CreateChannelGroup(ctx, CreateChannelGroupParams{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return g.ID
	}
	games, teams := group("NBA"), group("NBA EASTERN")
	stream := func(name string) int64 {
		id, err := q.UpsertProviderStream(ctx, UpsertProviderStreamParams{ProviderID: p.ID, ExternalID: name,
			Name: name, StreamUrl: "http://p/" + name})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	channel := func(name string, g int64, streams ...int64) int64 {
		c, err := q.CreateChannel(ctx, CreateChannelParams{Name: name, GroupID: &g})
		if err != nil {
			t.Fatal(err)
		}
		for i, s := range streams {
			if err := q.AddStreamToChannel(ctx, AddStreamToChannelParams{ChannelID: c.ID, ProviderStreamID: s,
				Priority: int32(i + 1)}); err != nil {
				t.Fatal(err)
			}
		}
		return c.ID
	}
	slotA, slotB := stream("Bucks vs Raptors :NBA 06 a"), stream("Bucks vs Raptors :NBA 06 b")
	tsn, att := stream("TSN 1"), stream("AT&T: TORONTO RAPTORS")
	slot := channel("NBA 06", games, slotA, slotB)
	raptors := channel("Toronto Raptors", teams, tsn, slotB, att) // slotB added by hand

	candidates := func() []int64 {
		t.Helper()
		rows, err := q.ListChannelCandidates(ctx, raptors)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, r := range rows {
			ids = append(ids, r.StreamID)
		}
		return ids
	}
	check := func(what string, want ...int64) {
		t.Helper()
		if got := candidates(); !slices.Equal(got, want) {
			t.Errorf("%s: candidates %v, want %v", what, got, want)
		}
	}
	check("no override", tsn, slotB, att)

	now := time.Now()
	set := func(start, end time.Time) {
		t.Helper()
		if err := q.UpsertEventOverride(ctx, UpsertEventOverrideParams{ChannelID: raptors, SourceChannelID: slot,
			Title: "Milwaukee Bucks vs Toronto Raptors", StartsAt: start, EndsAt: end}); err != nil {
			t.Fatal(err)
		}
	}
	set(now.Add(-time.Hour), now.Add(time.Hour))
	check("game on", slotA, slotB, tsn, att) // slotB once, in the slot's place

	// The dashboard's stream picker offers the same list, in the same
	// order, naming the slot for its streams.
	picker := func() []string {
		t.Helper()
		rows, err := q.LiveChannelStreams(ctx, []int64{raptors})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.StreamName+"|"+r.RoutedFrom)
		}
		return out
	}
	if got, want := picker(), []string{"Bucks vs Raptors :NBA 06 a|NBA 06", "Bucks vs Raptors :NBA 06 b|NBA 06",
		"TSN 1|", "AT&T: TORONTO RAPTORS|"}; !slices.Equal(got, want) {
		t.Errorf("game on: picker %q, want %q", got, want)
	}

	n, err := q.RemoveSlotLinksFromGroups(ctx, RemoveSlotLinksFromGroupsParams{
		TargetGroupIds: []int64{teams}, SourceGroupIds: []int64{games}})
	if err != nil || n != 1 {
		t.Fatalf("hand-added slot links removed: %d, %v; want 1", n, err)
	}
	check("game on, slot link gone", slotA, slotB, tsn, att)

	rows, err := q.ListEventOverrides(ctx)
	if err != nil || len(rows) != 1 || rows[0].SourceName != "NBA 06" || rows[0].ChannelID != raptors {
		t.Fatalf("ListEventOverrides = %+v, %v", rows, err)
	}

	set(now.Add(-5*time.Hour), now.Add(-time.Hour))
	check("game over", tsn, att)
	if got, want := picker(), []string{"TSN 1|", "AT&T: TORONTO RAPTORS|"}; !slices.Equal(got, want) {
		t.Errorf("game over: picker %q, want %q", got, want)
	}

	set(now.Add(-time.Hour), now.Add(time.Hour))
	if n, err := q.DeleteEventOverridesExcept(ctx, []int64{slot}); err != nil || n != 1 {
		t.Fatalf("DeleteEventOverridesExcept: %d, %v; want 1", n, err)
	}
	check("override dropped", tsn, att)
	if n, err := q.DeleteEventOverridesExcept(ctx, []int64{}); err != nil || n != 0 {
		t.Fatalf("DeleteEventOverridesExcept on none: %d, %v", n, err)
	}
}
