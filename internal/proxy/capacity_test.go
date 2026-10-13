package proxy

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestCapacityLimitAndRelease(t *testing.T) {
	c := NewCapacity()
	a, ok := c.TryAcquire(1, 2)
	if !ok {
		t.Fatal("first acquire refused")
	}
	if _, ok := c.TryAcquire(1, 2); !ok {
		t.Fatal("second acquire refused")
	}
	if _, ok := c.TryAcquire(1, 2); ok {
		t.Fatal("third acquire allowed past limit 2")
	}
	// Other providers are independent.
	if _, ok := c.TryAcquire(2, 1); !ok {
		t.Fatal("provider 2 refused")
	}

	a.Release()
	a.Release() // idempotent: must not free a second slot
	if got := c.InUse(1); got != 1 {
		t.Fatalf("in use after double release = %d, want 1", got)
	}
	if _, ok := c.TryAcquire(1, 2); !ok {
		t.Fatal("acquire after release refused")
	}
	if _, ok := c.TryAcquire(1, 2); ok {
		t.Fatal("double release leaked a slot")
	}
	if snap := c.Snapshot(); snap[1] != 2 || snap[2] != 1 || len(snap) != 2 {
		t.Errorf("snapshot = %v", snap)
	}
}

func TestCapacityUnlimitedAndLoweredLimit(t *testing.T) {
	c := NewCapacity()
	var slots []*Slot
	for range 50 {
		s, ok := c.TryAcquire(7, 0) // 0 = unlimited
		if !ok {
			t.Fatal("unlimited provider refused")
		}
		slots = append(slots, s)
	}
	// Admin lowers the limit to 10: existing 50 stay, new ones refused
	// until use drops below 10.
	if _, ok := c.TryAcquire(7, 10); ok {
		t.Fatal("acquire allowed above lowered limit")
	}
	for _, s := range slots[:41] {
		s.Release()
	}
	if _, ok := c.TryAcquire(7, 10); !ok {
		t.Fatal("acquire refused at 9/10")
	}
	for _, s := range slots[41:] {
		s.Release()
	}
	if got := c.InUse(7); got != 1 {
		t.Errorf("in use = %d, want 1", got)
	}
}

// TestCapacityConcurrentNeverExceeds is the 4.3 AC: many goroutines racing
// to acquire and release never hold more than the limit at once.
func TestCapacityConcurrentNeverExceeds(t *testing.T) {
	const (
		limit   = 3
		workers = 64
		rounds  = 500
	)
	c := NewCapacity()
	var held, peak, granted atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range rounds {
				s, ok := c.TryAcquire(1, limit)
				if !ok {
					continue
				}
				granted.Add(1)
				n := held.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				if got := c.InUse(1); got > limit {
					t.Errorf("InUse = %d, limit %d", got, limit)
				}
				held.Add(-1)
				s.Release()
				s.Release() // racing double release must stay harmless
			}
		})
	}
	wg.Wait()
	if peak.Load() > limit {
		t.Errorf("peak held %d > limit %d", peak.Load(), limit)
	}
	if granted.Load() == 0 {
		t.Error("no acquire ever succeeded")
	}
	if got := c.InUse(1); got != 0 {
		t.Errorf("leaked %d slots", got)
	}
}
