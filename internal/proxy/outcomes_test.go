package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

type fakeOutcomeStore struct {
	mu   sync.Mutex
	rows []db.UpsertOutcomeBucketParams
	err  error
}

func (f *fakeOutcomeStore) UpsertOutcomeBucket(_ context.Context, arg db.UpsertOutcomeBucketParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, arg)
	return nil
}

func TestOutcomesAccumulateAndFlush(t *testing.T) {
	store := &fakeOutcomeStore{}
	o := NewOutcomes(store, nil)
	o.Record(7, 0, "Strong8K", OutcomeOpenOK)
	o.Record(7, 0, "Strong8K", OutcomeOpenOK)
	o.Record(7, 0, "Strong8K", OutcomeOpenFailed)
	o.Record(8, 0, "", OutcomeProbeFail)

	o.flush(context.Background())

	if len(store.rows) != 3 {
		t.Fatalf("flushed %d rows, want 3 (one per provider, backbone and outcome)", len(store.rows))
	}
	got := map[string]int32{}
	for _, r := range store.rows {
		got[r.Outcome+"/"+r.Backbone] = r.Count
	}
	if got[OutcomeOpenOK+"/Strong8K"] != 2 || got[OutcomeOpenFailed+"/Strong8K"] != 1 || got[OutcomeProbeFail+"/"] != 1 {
		t.Fatalf("counts = %v", got)
	}
	if n := len(o.counts); n != 0 {
		t.Fatalf("%d counts left after flush, want none", n)
	}
}

func TestOutcomesIgnoreNilAndUnknownProvider(t *testing.T) {
	var none *Outcomes
	none.Record(1, 0, "", OutcomeOpenOK) // must not panic

	store := &fakeOutcomeStore{}
	o := NewOutcomes(store, nil)
	o.Record(0, 0, "", OutcomeOpenOK)
	o.flush(context.Background())
	if len(store.rows) != 0 {
		t.Fatalf("recorded %d rows for provider 0, want none", len(store.rows))
	}
}

func TestOutcomesFlushDropsFailedWrites(t *testing.T) {
	store := &fakeOutcomeStore{err: errors.New("database down")}
	o := NewOutcomes(store, nil)
	o.Record(1, 0, "", OutcomeOpenOK)
	o.flush(context.Background())

	store.err = nil
	o.flush(context.Background())
	if len(store.rows) != 0 {
		t.Fatalf("a failed write was retried with %d rows, want it dropped", len(store.rows))
	}
}

func TestOutcomesKeepStreamsApart(t *testing.T) {
	store := &fakeOutcomeStore{}
	o := NewOutcomes(store, nil)
	o.Record(7, 101, "Strong8K", OutcomeOpenOK)
	o.Record(7, 102, "Strong8K", OutcomeOpenFailed)
	o.Record(7, 0, "Strong8K", OutcomeProbeOK)

	o.flush(context.Background())

	streams := map[int64]bool{}
	for _, r := range store.rows {
		streams[r.StreamID] = true
	}
	if len(store.rows) != 3 || !streams[101] || !streams[102] || !streams[0] {
		t.Fatalf("rows = %+v, want one row per stream (101, 102) and one for the provider probe (0)", store.rows)
	}
}
