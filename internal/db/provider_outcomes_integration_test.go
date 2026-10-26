//go:build integration

package db

import (
	"context"
	"testing"
	"time"
)

// TestOutcomeBuckets covers the upsert that accumulates a minute's counts,
// the hourly sums the health window reads, the window's cutoff, and pruning.
func TestOutcomeBuckets(t *testing.T) {
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

	minute := time.Now().UTC().Truncate(time.Hour).Add(5 * time.Minute)
	upsert := func(at time.Time, stream int64, backbone, outcome string, n int32) {
		t.Helper()
		if err := q.UpsertOutcomeBucket(ctx, UpsertOutcomeBucketParams{BucketStart: at, ProviderID: 7,
			StreamID: stream, Backbone: backbone, Outcome: outcome, Count: n}); err != nil {
			t.Fatal(err)
		}
	}
	upsert(minute, 0, "Strong8K", "open_ok", 2)
	upsert(minute, 0, "Strong8K", "open_ok", 3) // same key: counts add up
	upsert(minute.Add(time.Minute), 0, "Strong8K", "open_ok", 4)
	upsert(minute.Add(time.Minute), 0, "Strong8K", "open_failed", 1)
	upsert(minute.Add(time.Minute), 9, "Strong8K", "open_ok", 1)         // a stream of the same provider: its own row
	upsert(minute.Add(-8*24*time.Hour), 0, "Strong8K", "open_failed", 9) // outside the window

	since := minute.Add(-7 * 24 * time.Hour)
	rows, err := q.ListOutcomeHoursSince(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.Outcome] += r.Count
		if !r.HourStart.Equal(minute.Truncate(time.Hour)) {
			t.Errorf("hour_start = %v, want the hour of %v", r.HourStart, minute)
		}
	}
	if len(rows) != 3 || got["open_ok"] != 10 || got["open_failed"] != 1 {
		t.Fatalf("hourly rows = %+v, want open_ok 10 and open_failed 1 within the window", rows)
	}

	n, err := q.PruneOutcomeBuckets(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want the one outside the window", n)
	}
}
