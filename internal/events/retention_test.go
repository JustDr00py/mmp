package events

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

type fakeStore struct {
	settings map[string][]byte
	before   []time.Time
	batches  []int64 // rows each PruneEvents call reports
}

func (f *fakeStore) GetSetting(_ context.Context, key string) ([]byte, error) {
	if v, ok := f.settings[key]; ok {
		return v, nil
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeStore) SetSetting(_ context.Context, arg db.SetSettingParams) error {
	f.settings[arg.Key] = arg.Value
	return nil
}

func (f *fakeStore) PruneOutcomeBuckets(context.Context, time.Time) (int64, error) { return 0, nil }

func (f *fakeStore) PruneEvents(_ context.Context, before time.Time) (int64, error) {
	f.before = append(f.before, before)
	if len(f.batches) == 0 {
		return 0, nil
	}
	n := f.batches[0]
	f.batches = f.batches[1:]
	return n, nil
}

func (f *fakeStore) PruneViewingSessions(context.Context, time.Time) (int64, error) { return 0, nil }
func (f *fakeStore) PruneUserNetworks(context.Context, time.Time) (int64, error)    { return 0, nil }
func (f *fakeStore) PruneAuthFailures(context.Context, time.Time) (int64, error)    { return 0, nil }
func (f *fakeStore) PruneAudit(context.Context, time.Time) (int64, error)           { return 0, nil }

func TestPrune(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	f := &fakeStore{settings: map[string][]byte{}, batches: []int64{10000, 10000, 42}}
	n, err := Prune(ctx, f, now)
	if err != nil || n != 20042 {
		t.Fatalf("Prune = %d, %v; want 20042 in batches", n, err)
	}
	if len(f.before) != 4 || !f.before[0].Equal(now.AddDate(0, 0, -30)) {
		t.Fatalf("cutoffs = %v, want 30 days back until a batch is empty", f.before)
	}

	if err := SaveRetention(ctx, f, Retention{KeepDays: 0}); err == nil {
		t.Fatal("0 days accepted")
	}
	if err := SaveRetention(ctx, f, Retention{KeepDays: 7}); err != nil {
		t.Fatal(err)
	}
	if r, _ := LoadRetention(ctx, f); r.KeepDays != 7 {
		t.Fatalf("loaded %+v", r)
	}
	f.before = nil
	if _, err := Prune(ctx, f, now); err != nil || !f.before[0].Equal(now.AddDate(0, 0, -7)) {
		t.Fatalf("cutoff after saving 7 days = %v (%v)", f.before, err)
	}
}
