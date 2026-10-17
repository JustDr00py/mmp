// Package activity records what customers do, for their user pages: the
// client networks they authenticate from (so "last seen"), the live
// channels they watch, and wrong passwords. Writes happen off the request
// path; a full queue drops the record rather than slow a player down.
package activity

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
)

// Store persists activity (satisfied by *db.Queries).
type Store interface {
	TouchUserNetwork(ctx context.Context, arg db.TouchUserNetworkParams) error
	InsertViewingSession(ctx context.Context, arg db.InsertViewingSessionParams) error
	InsertAuthFailure(ctx context.Context, arg db.InsertAuthFailureParams) error
}

// seenEvery is how often one user's network is written: players
// authenticate on every request, the page only needs the minute.
const seenEvery = time.Minute

// maxSeen bounds the throttle table.
const maxSeen = 10000

type seenKey struct {
	user    int64
	network string
}

// Tracker queues activity for a single writer. A nil Tracker records
// nothing.
type Tracker struct {
	q       Store
	log     *slog.Logger
	now     func() time.Time
	ch      chan func(context.Context) error
	dropped atomic.Uint64

	mu   sync.Mutex
	seen map[seenKey]time.Time
}

// New builds a Tracker with room for buf queued records.
func New(q Store, log *slog.Logger, buf int) *Tracker {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Tracker{q: q, log: log, now: time.Now, ch: make(chan func(context.Context) error, max(buf, 1)),
		seen: map[seenKey]time.Time{}}
}

// Seen notes that userID authenticated from ip (in client network
// network). Repeats within seenEvery are not written.
func (t *Tracker) Seen(userID int64, ip, network string) {
	if t == nil || userID == 0 {
		return
	}
	now := t.now()
	k := seenKey{userID, network}
	t.mu.Lock()
	if last, ok := t.seen[k]; ok && now.Sub(last) < seenEvery {
		t.mu.Unlock()
		return
	}
	if len(t.seen) >= maxSeen {
		for k, at := range t.seen {
			if now.Sub(at) >= seenEvery {
				delete(t.seen, k)
			}
		}
		if len(t.seen) >= maxSeen {
			t.seen = map[seenKey]time.Time{}
		}
	}
	t.seen[k] = now
	t.mu.Unlock()
	t.queue(func(ctx context.Context) error {
		return t.q.TouchUserNetwork(ctx, db.TouchUserNetworkParams{UserID: userID, Network: network, Ip: ip, At: now})
	})
}

// Watched records a finished live viewing session.
func (t *Tracker) Watched(userID, channelID int64, ip, output string, start, end time.Time) {
	if t == nil || userID == 0 {
		return
	}
	t.queue(func(ctx context.Context) error {
		return t.q.InsertViewingSession(ctx, db.InsertViewingSessionParams{UserID: userID, ChannelID: channelID,
			Ip: ip, Output: output, StartedAt: start, EndedAt: end})
	})
}

// AuthFailed records a wrong password for username from ip.
func (t *Tracker) AuthFailed(username, ip string) {
	if t == nil {
		return
	}
	at := t.now()
	t.queue(func(ctx context.Context) error {
		return t.q.InsertAuthFailure(ctx, db.InsertAuthFailureParams{Ts: at, Username: username, Ip: ip})
	})
}

// Dropped is how many records were lost to a full queue.
func (t *Tracker) Dropped() uint64 {
	if t == nil {
		return 0
	}
	return t.dropped.Load()
}

func (t *Tracker) queue(write func(context.Context) error) {
	select {
	case t.ch <- write:
	default:
		if t.dropped.Add(1)%1000 == 1 {
			t.log.Warn("activity record dropped: queue full", "dropped", t.dropped.Load())
		}
	}
}

// Run writes queued records until ctx ends, then drains what is left with
// a short deadline.
func (t *Tracker) Run(ctx context.Context) {
	base := context.WithoutCancel(ctx)
	write := func(ctx context.Context, w func(context.Context) error) {
		if err := w(ctx); err != nil {
			t.log.Warn("activity record failed", "err", err)
		}
	}
	for {
		select {
		case w := <-t.ch:
			wctx, cancel := context.WithTimeout(base, 5*time.Second)
			write(wctx, w)
			cancel()
		case <-ctx.Done():
			drainCtx, cancel := context.WithTimeout(base, 2*time.Second)
			defer cancel()
			for {
				select {
				case w := <-t.ch:
					write(drainCtx, w)
				default:
					return
				}
			}
		}
	}
}
