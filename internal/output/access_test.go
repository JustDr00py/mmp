package output

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/proxy"
)

// TestAccessRejectsOverLimit is the 5.3 AC: the (max+1)th connection is
// rejected, and a released slot can be reused.
func TestAccessRejectsOverLimit(t *testing.T) {
	a := NewAccess(nil)
	u := db.User{ID: 1, MaxConnections: 2}
	other := db.User{ID: 2, MaxConnections: 1}

	s1, err := a.Acquire(u)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Acquire(u); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Acquire(u); !errors.Is(err, ErrTooManyConnections) {
		t.Fatalf("3rd connection with max 2: err = %v, want ErrTooManyConnections", err)
	}
	if _, err := a.Acquire(other); err != nil {
		t.Errorf("limits must be per user: %v", err)
	}
	s1.Release()
	s1.Release() // idempotent
	if a.ActiveConnections(u.ID) != 1 {
		t.Fatalf("active = %d after release", a.ActiveConnections(u.ID))
	}
	if _, err := a.Acquire(u); err != nil {
		t.Errorf("slot not reusable after release: %v", err)
	}
	if c := a.Connections(); c[1] != 2 || c[2] != 1 {
		t.Errorf("connections = %v", c)
	}

	unlimited := db.User{ID: 3, MaxConnections: 0}
	for range 20 {
		if _, err := a.Acquire(unlimited); err != nil {
			t.Fatalf("max_connections 0 must be unlimited: %v", err)
		}
	}
}

// TestAccessConcurrentLimit: many simultaneous stream starts for one user
// never exceed max_connections.
func TestAccessConcurrentLimit(t *testing.T) {
	a := NewAccess(nil)
	u := db.User{ID: 9, MaxConnections: 3}
	var granted atomic.Int32
	var mu sync.Mutex
	var slots []*proxy.Slot
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			s, err := a.Acquire(u)
			if err != nil {
				if !errors.Is(err, ErrTooManyConnections) {
					t.Error(err)
				}
				return
			}
			granted.Add(1)
			mu.Lock()
			slots = append(slots, s)
			mu.Unlock()
		})
	}
	wg.Wait()
	if g := granted.Load(); g != 3 {
		t.Errorf("granted %d concurrent streams, want exactly 3", g)
	}
	for _, s := range slots {
		s.Release()
	}
	if n := a.ActiveConnections(u.ID); n != 0 {
		t.Errorf("leaked %d slots", n)
	}
}
