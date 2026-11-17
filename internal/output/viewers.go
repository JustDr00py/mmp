package output

import (
	"cmp"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Viewer is one customer connection, for the dashboard: a live channel's TS
// connection or HLS lease, or a movie/episode playback.
type Viewer struct {
	ID        uint64    `json:"id"`
	ChannelID int64     `json:"channel_id"` // 0 for VOD
	UserID    int64     `json:"user_id"`
	Username  string    `json:"username"`
	Addr      string    `json:"addr"`   // client IP
	Output    string    `json:"output"` // "ts", "hls", "preview" (admin preview player), "movie" or "series"
	Since     time.Time `json:"since"`

	// VOD playback only.
	Title      string `json:"title,omitempty"`
	ItemID     int64  `json:"item_id,omitempty"` // movie or episode id
	ProviderID int64  `json:"provider_id,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"` // sent to the player so far
	// IdleSince is set while the player has no request open (it buffered
	// ahead or paused) and the line is held for it; zero while downloading.
	IdleSince time.Time `json:"idle_since,omitzero"`
}

// IsVOD reports a movie or episode playback.
func (v Viewer) IsVOD() bool { return v.Output == "movie" || v.Output == "series" }

// viewers tracks who is watching and how to disconnect them.
type viewers struct {
	mu    sync.Mutex
	next  uint64
	m     map[uint64]*viewerEntry
	ended func(v Viewer, at time.Time) // optional: a viewer left
}

type viewerEntry struct {
	Viewer
	kick func()
	live *viewerLive // optional
}

// viewerLive is what a viewer's entry reads live from its connection.
type viewerLive struct {
	bytes     *atomic.Int64 // Viewer.Bytes
	idleSince *atomic.Int64 // Viewer.IdleSince, unix nanoseconds; 0 while downloading
}

// add registers v; kick must end the connection (and with it, through the
// caller's cleanup, call remove). live may be nil. It returns remove, which
// is idempotent.
func (vs *viewers) add(v Viewer, kick func(), live *viewerLive) (uint64, func()) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if vs.m == nil {
		vs.m = map[uint64]*viewerEntry{}
	}
	vs.next++
	v.ID = vs.next
	e := &viewerEntry{Viewer: v, kick: kick, live: live}
	vs.m[v.ID] = e
	id := v.ID
	return id, func() {
		vs.mu.Lock()
		e, ok := vs.m[id]
		delete(vs.m, id)
		ended := vs.ended
		vs.mu.Unlock()
		if ok && ended != nil {
			ended(e.Viewer, time.Now())
		}
	}
}

func (vs *viewers) list() []Viewer {
	vs.mu.Lock()
	out := make([]Viewer, 0, len(vs.m))
	for _, e := range vs.m {
		v := e.Viewer
		if e.live != nil && e.live.bytes != nil {
			v.Bytes = e.live.bytes.Load()
		}
		if e.live != nil && e.live.idleSince != nil {
			if n := e.live.idleSince.Load(); n != 0 {
				v.IdleSince = time.Unix(0, n)
			}
		}
		out = append(out, v)
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
