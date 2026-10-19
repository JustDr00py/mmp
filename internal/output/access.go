// Package output serves customer-facing endpoints: M3U playlists, XMLTV,
// the Xtream API, and live streams.
package output

import (
	"errors"
	"time"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/proxy"
)

// ErrTooManyConnections means the user already has max_connections streams.
var ErrTooManyConnections = errors.New("connection limit reached")

// Access combines customer authentication with per-user stream accounting.
type Access struct {
	Customers *auth.Customers
	conns     *proxy.Capacity // keyed by user id; same slot semantics as providers
	viewers   viewers
}

// NewAccess builds the customer access layer.
func NewAccess(c *auth.Customers) *Access {
	return &Access{Customers: c, conns: proxy.NewCapacity()}
}

// Acquire reserves one of u's concurrent stream slots; release it when the
// stream ends. max_connections 0 means unlimited.
func (a *Access) Acquire(u db.User) (*proxy.Slot, error) {
	s, ok := a.conns.TryAcquire(u.ID, int(u.MaxConnections))
	if !ok {
		return nil, ErrTooManyConnections
	}
	return s, nil
}

// ActiveConnections is the number of streams u has open.
func (a *Access) ActiveConnections(userID int64) int { return a.conns.InUse(userID) }

// Connections returns open streams per user, for the dashboard.
func (a *Access) Connections() map[int64]int { return a.conns.Snapshot() }

// Viewers lists the customers watching live channels, oldest first.
func (a *Access) Viewers() []Viewer { return a.viewers.list() }

// OnViewerEnd calls fn each time a viewer leaves (for watch history).
func (a *Access) OnViewerEnd(fn func(v Viewer, at time.Time)) {
	a.viewers.mu.Lock()
	a.viewers.ended = fn
	a.viewers.mu.Unlock()
}

// Kick disconnects viewer id. Its player may reconnect; this does not lock
// the customer out. It reports whether the viewer was found.
func (a *Access) Kick(id uint64) bool { return a.viewers.kick(id) }

// Watch lists a connection that holds no customer slot (the admin preview
// player) with the viewers; kick must end it. Call the returned remove when
// it ends.
func (a *Access) Watch(v Viewer, kick func()) (remove func()) {
	_, remove = a.viewers.add(v, kick)
	return remove
}
