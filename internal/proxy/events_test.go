package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sysadmin/mmp/internal/db"
)

type fakeInserter struct {
	mu    sync.Mutex
	rows  []db.InsertEventParams
	block chan struct{} // when non-nil, inserts wait on it (a hung database)
}

func (f *fakeInserter) InsertEvent(ctx context.Context, arg db.InsertEventParams) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err // like a real driver: a cancelled ctx loses the row
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, arg)
	return nil
}

func (f *fakeInserter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func TestEventRecorderPersistsWithNulls(t *testing.T) {
	f := &fakeInserter{}
	r := NewEventRecorder(f, nil, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	at := time.Now()
	r.Record(Event{Type: EventFailover, At: at, ChannelID: 7, ProviderID: 2, FromStreamID: 11, ToStreamID: 22, Message: "switched upstream"})
	r.Record(Event{Type: EventExhausted, At: at, ChannelID: 7, Message: "all upstream candidates exhausted"})
	eventually(t, 2*time.Second, func() bool { return f.count() == 2 }, "events not inserted")
	cancel()
	<-done

	got := f.rows[0]
	if got.Type != EventFailover || !got.Ts.Equal(at) || *got.ChannelID != 7 || *got.ProviderID != 2 ||
		*got.FromStreamID != 11 || *got.ToStreamID != 22 || got.Message != "switched upstream" {
		t.Errorf("failover row = %+v", got)
	}
	ex := f.rows[1]
	if ex.ProviderID != nil || ex.FromStreamID != nil || ex.ToStreamID != nil || *ex.ChannelID != 7 {
		t.Errorf("unset ids must be NULL: %+v", ex)
	}
}

// TestEventRecorderNeverBlocksSession: with the database hung, Record still
// returns immediately; overflow is dropped and counted.
func TestEventRecorderNeverBlocksSession(t *testing.T) {
	f := &fakeInserter{block: make(chan struct{})}
	r := NewEventRecorder(f, nil, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	start := time.Now()
	for range 100 {
		r.Record(Event{Type: EventOpenFailed, ChannelID: 1, At: time.Now()})
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("Record blocked for %v", d)
	}
	// At most 1 in flight + 4 queued are kept; the rest are dropped.
	if d := r.Dropped(); d < 95 || d > 96 {
		t.Errorf("dropped = %d, want 95-96", d)
	}
	close(f.block)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// TestEventRecorderDrainsOnShutdown: events queued before shutdown are
// still written.
func TestEventRecorderDrainsOnShutdown(t *testing.T) {
	f := &fakeInserter{}
	r := NewEventRecorder(f, nil, 32)
	for i := range 10 {
		r.Record(Event{Type: EventFailover, ChannelID: int64(i + 1), At: time.Now()})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already shutting down when Run starts
	r.Run(ctx)
	if n := f.count(); n != 10 {
		t.Errorf("drained %d events, want 10", n)
	}
}
