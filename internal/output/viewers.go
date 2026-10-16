package output

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

// Viewer is one customer watching a live channel, for the dashboard: a TS
// connection or an HLS lease.
type Viewer struct {
	ID        uint64    `json:"id"`
	ChannelID int64     `json:"channel_id"`
	UserID    int64     `json:"user_id"`
	Username  string    `json:"username"`
	Addr      string    `json:"addr"`   // client IP
	Output    string    `json:"output"` // "ts" or "hls"
	Since     time.Time `json:"since"`
}

// viewers tracks who is watching and how to disconnect them.
type viewers struct {
	mu   sync.Mutex
	next uint64
	m    map[uint64]*viewerEntry
}

type viewerEntry struct {
	Viewer
	kick func()
}

// add registers v; kick must end the connection (and with it, through the
// caller's cleanup, call remove). It returns remove, which is idempotent.
func (vs *viewers) add(v Viewer, kick func()) (uint64, func()) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if vs.m == nil {
		vs.m = map[uint64]*viewerEntry{}
	}
	vs.next++
	v.ID = vs.next
	vs.m[v.ID] = &viewerEntry{Viewer: v, kick: kick}
	id := v.ID
	return id, func() {
		vs.mu.Lock()
		delete(vs.m, id)
		vs.mu.Unlock()
	}
}

func (vs *viewers) list() []Viewer {
	vs.mu.Lock()
	out := make([]Viewer, 0, len(vs.m))
	for _, e := range vs.m {
		out = append(out, e.Viewer)
	}
	vs.mu.Unlock()
	slices.SortFunc(out, func(a, b Viewer) int { return cmp.Or(a.Since.Compare(b.Since), cmp.Compare(a.ID, b.ID)) })
	return out
}

func (vs *viewers) kick(id uint64) bool {
	vs.mu.Lock()
	e, ok := vs.m[id]
	vs.mu.Unlock()
	if ok {
		e.kick()
	}
	return ok
}
