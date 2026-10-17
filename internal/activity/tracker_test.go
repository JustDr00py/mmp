package activity

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
)

type fakeStore struct {
	mu       sync.Mutex
	networks []db.TouchUserNetworkParams
	watched  []db.InsertViewingSessionParams
	failures []db.InsertAuthFailureParams
}

func (f *fakeStore) TouchUserNetwork(_ context.Context, a db.TouchUserNetworkParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks = append(f.networks, a)
	return nil
}

func (f *fakeStore) InsertViewingSession(_ context.Context, a db.InsertViewingSessionParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watched = append(f.watched, a)
	return nil
}

func (f *fakeStore) InsertAuthFailure(_ context.Context, a db.InsertAuthFailureParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, a)
	return nil
}

func TestTracker(t *testing.T) {
	f := &fakeStore{}
	tr := New(f, nil, 100)
	clock := time.Unix(1700000000, 0)
	tr.now = func() time.Time { return clock }

	tr.Seen(1, "203.0.113.7", "203.0.113.7")
	tr.Seen(1, "203.0.113.7", "203.0.113.7")   // within a minute: not written
	tr.Seen(1, "2001:db8::1", "2001:db8::/64") // another network is
	tr.Seen(2, "203.0.113.7", "203.0.113.7")   // and another user
	tr.Seen(0, "203.0.113.7", "203.0.113.7")   // no user: ignored
	clock = clock.Add(seenEvery)
	tr.Seen(1, "203.0.113.8", "203.0.113.8") // new address
	tr.Seen(1, "203.0.113.7", "203.0.113.7") // a minute later: written again
	tr.Watched(1, 10, "203.0.113.7", "ts", clock.Add(-time.Hour), clock)
	tr.AuthFailed("alice", "198.51.100.1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run drains the queue on its way out
	tr.Run(ctx)

	if len(f.networks) != 5 {
		t.Errorf("network writes = %d, want 5: %+v", len(f.networks), f.networks)
	}
	if len(f.watched) != 1 || f.watched[0].ChannelID != 10 || f.watched[0].EndedAt.Sub(f.watched[0].StartedAt) != time.Hour {
		t.Errorf("watched = %+v", f.watched)
	}
	if len(f.failures) != 1 || f.failures[0].Username != "alice" || f.failures[0].Ip != "198.51.100.1" {
		t.Errorf("failures = %+v", f.failures)
	}

	var nilTracker *Tracker // no-op
	nilTracker.Seen(1, "x", "x")
	nilTracker.Watched(1, 1, "x", "ts", clock, clock)
	nilTracker.AuthFailed("x", "x")
}

func TestTrackerQueueFullDrops(t *testing.T) {
	tr := New(&fakeStore{}, nil, 1)
	tr.AuthFailed("a", "1")
	tr.AuthFailed("b", "1") // nobody is writing: the queue is full
	if tr.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", tr.Dropped())
	}
}
