package proxy

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/sysadmin/mmp/internal/db"
)

// EventInserter persists events (satisfied by *db.Queries).
type EventInserter interface {
	InsertEvent(ctx context.Context, arg db.InsertEventParams) error
}

// EventRecorder logs session events and writes them to the events table
// off the streaming path: Record never blocks a session on the database.
type EventRecorder struct {
	q       EventInserter
	log     *slog.Logger
	ch      chan Event
	dropped atomic.Uint64
}

// NewEventRecorder buffers up to buf events awaiting insert.
func NewEventRecorder(q EventInserter, log *slog.Logger, buf int) *EventRecorder {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &EventRecorder{q: q, log: log, ch: make(chan Event, max(buf, 1))}
}

// Record logs e and queues it for insert. If the queue is full (database
// slow or down) the row is dropped and counted; the log line still exists.
func (r *EventRecorder) Record(e Event) {
	r.log.Info("proxy event", "type", e.Type, "channel_id", e.ChannelID, "provider_id", e.ProviderID,
		"from_stream_id", e.FromStreamID, "to_stream_id", e.ToStreamID, "message", e.Message)
	select {
	case r.ch <- e:
	default:
		r.dropped.Add(1)
		r.log.Warn("proxy event dropped: queue full", "type", e.Type, "channel_id", e.ChannelID)
	}
}

// Dropped is how many events were not persisted because the queue was full.
func (r *EventRecorder) Dropped() uint64 { return r.dropped.Load() }

// Run inserts queued events until ctx ends, then drains what is left with
// a short deadline so shutdown cannot hang on a dead database. Inserts do
// not inherit ctx's cancellation: an event dequeued as shutdown begins is
// still written.
func (r *EventRecorder) Run(ctx context.Context) {
	base := context.WithoutCancel(ctx)
	for {
		select {
		case e := <-r.ch:
			ictx, cancel := context.WithTimeout(base, 5*time.Second)
			r.insert(ictx, e)
			cancel()
		case <-ctx.Done():
			drainCtx, cancel := context.WithTimeout(base, 2*time.Second)
			defer cancel()
			for {
				select {
				case e := <-r.ch:
					r.insert(drainCtx, e)
				default:
					return
				}
			}
		}
	}
}

func (r *EventRecorder) insert(ctx context.Context, e Event) {
	if err := r.q.InsertEvent(ctx, db.InsertEventParams{
		Ts:           e.At,
		Type:         e.Type,
		ChannelID:    nonZero(e.ChannelID),
		ProviderID:   nonZero(e.ProviderID),
		FromStreamID: nonZero(e.FromStreamID),
		ToStreamID:   nonZero(e.ToStreamID),
		Message:      e.Message,
	}); err != nil {
		r.log.Warn("proxy event insert failed", "type", e.Type, "channel_id", e.ChannelID, "err", err)
	}
}

// nonZero maps an unset id to SQL NULL.
func nonZero(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}
