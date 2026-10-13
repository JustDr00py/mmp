package proxy

import (
	"maps"
	"sync"
	"sync/atomic"
)

// Capacity accounts provider connection slots. Slots are a scarce, paid
// resource: every open upstream connection holds exactly one, and a
// provider never has more than its max_connections in use.
type Capacity struct {
	mu   sync.Mutex
	used map[int64]int
}

// NewCapacity returns an empty slot table.
func NewCapacity() *Capacity {
	return &Capacity{used: map[int64]int{}}
}

// TryAcquire takes a slot at providerID if fewer than limit are in use.
// limit is the provider's current max_connections, passed per call so admin
// edits apply immediately; 0 means unlimited. It never blocks: a full
// provider returns ok=false and the caller moves to its next candidate.
// Lowering a limit below current use only refuses new acquires.
func (c *Capacity) TryAcquire(providerID int64, limit int) (*Slot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit > 0 && c.used[providerID] >= limit {
		return nil, false
	}
	c.used[providerID]++
	return &Slot{c: c, providerID: providerID}, true
}

// InUse is the number of slots held at providerID.
func (c *Capacity) InUse(providerID int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used[providerID]
}

// Snapshot returns slots in use per provider (providers at zero omitted).
func (c *Capacity) Snapshot() map[int64]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.used)
}

func (c *Capacity) release(providerID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used[providerID] <= 1 {
		delete(c.used, providerID)
		return
	}
	c.used[providerID]--
}

// Slot is one held provider connection slot.
type Slot struct {
	c          *Capacity
	providerID int64
	released   atomic.Bool
}

// ProviderID is the provider this slot belongs to.
func (s *Slot) ProviderID() int64 { return s.providerID }

// Release returns the slot. It is idempotent, so failover and teardown
// paths can both release without double-freeing.
func (s *Slot) Release() {
	if s.released.CompareAndSwap(false, true) {
		s.c.release(s.providerID)
	}
}
