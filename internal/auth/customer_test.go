package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/mmp/internal/db"
)

// fakeUsers is an in-memory CustomerQuerier.
type fakeUsers struct {
	mu    sync.Mutex
	users map[string]db.User // by username
}

func (f *fakeUsers) GetUserByUsername(_ context.Context, name string) (db.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[name]
	if !ok {
		return db.User{}, pgx.ErrNoRows
	}
	return u, nil
}

func (f *fakeUsers) GetUserByStreamToken(_ context.Context, tok string) (db.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.StreamToken == tok {
			return u, nil
		}
	}
	return db.User{}, pgx.ErrNoRows
}

func (f *fakeUsers) update(name string, fn func(*db.User)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.users[name]
	fn(&u)
	f.users[name] = u
}

func newFakeUsers(t *testing.T) *fakeUsers {
	t.Helper()
	h, err := HashPassword("alice-pw1")
	if err != nil {
		t.Fatal(err)
	}
	return &fakeUsers{users: map[string]db.User{
		"alice": {ID: 1, Username: "alice", PasswordHash: h, Role: RoleCustomer, Enabled: true, StreamToken: "tok-alice", MaxConnections: 1},
	}}
}

func TestCustomersByPassword(t *testing.T) {
	f := newFakeUsers(t)
	c := NewCustomers(f, time.Minute)
	ctx := context.Background()

	u, err := c.ByPassword(ctx, "alice", "alice-pw1")
	if err != nil || u.ID != 1 {
		t.Fatalf("good creds = %v %v", u, err)
	}
	for name, creds := range map[string][2]string{
		"wrong password": {"alice", "nope-nope"},
		"unknown user":   {"ghost", "alice-pw1"},
		"empty":          {"", ""},
	} {
		if _, err := c.ByPassword(ctx, creds[0], creds[1]); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: err = %v, want ErrUnauthorized", name, err)
		}
	}
	if len(c.cache) != 1 {
		t.Errorf("cache has %d entries; failures must not be cached", len(c.cache))
	}
}

// TestCustomersCacheNeverOutlivesAccountChanges: the cache only skips the
// bcrypt comparison; password changes, disables and expiry apply at once.
func TestCustomersCacheNeverOutlivesAccountChanges(t *testing.T) {
	ctx := context.Background()

	t.Run("password change", func(t *testing.T) {
		f := newFakeUsers(t)
		c := NewCustomers(f, time.Hour)
		if _, err := c.ByPassword(ctx, "alice", "alice-pw1"); err != nil {
			t.Fatal(err)
		}
		h2, _ := HashPassword("alice-pw2")
		f.update("alice", func(u *db.User) { u.PasswordHash = h2 })
		if _, err := c.ByPassword(ctx, "alice", "alice-pw1"); !errors.Is(err, ErrUnauthorized) {
			t.Error("old password still accepted from cache after change")
		}
		if _, err := c.ByPassword(ctx, "alice", "alice-pw2"); err != nil {
			t.Errorf("new password: %v", err)
		}
	})
	t.Run("disable", func(t *testing.T) {
		f := newFakeUsers(t)
		c := NewCustomers(f, time.Hour)
		c.ByPassword(ctx, "alice", "alice-pw1") //nolint:errcheck // warm cache
		f.update("alice", func(u *db.User) { u.Enabled = false })
		if _, err := c.ByPassword(ctx, "alice", "alice-pw1"); !errors.Is(err, ErrUnauthorized) {
			t.Error("disabled user accepted via cache")
		}
		if _, err := c.ByToken(ctx, "tok-alice"); !errors.Is(err, ErrUnauthorized) {
			t.Error("disabled user accepted via token")
		}
	})
	t.Run("expiry", func(t *testing.T) {
		f := newFakeUsers(t)
		c := NewCustomers(f, time.Hour)
		c.ByPassword(ctx, "alice", "alice-pw1") //nolint:errcheck // warm cache
		past := time.Now().Add(-time.Second)
		f.update("alice", func(u *db.User) { u.ExpiresAt = &past })
		if _, err := c.ByPassword(ctx, "alice", "alice-pw1"); !errors.Is(err, ErrUnauthorized) {
			t.Error("expired user accepted via cache")
		}
	})
	t.Run("ttl", func(t *testing.T) {
		f := newFakeUsers(t)
		c := NewCustomers(f, time.Minute)
		now := time.Now()
		c.now = func() time.Time { return now }
		c.ByPassword(ctx, "alice", "alice-pw1") //nolint:errcheck // warm cache
		key := c.keyFor("alice", "alice-pw1")
		first := c.cache[key].at
		now = now.Add(2 * time.Minute) // past ttl: re-verified, entry refreshed
		if _, err := c.ByPassword(ctx, "alice", "alice-pw1"); err != nil {
			t.Fatal(err)
		}
		if !c.cache[key].at.After(first) {
			t.Error("stale cache entry not re-verified")
		}
	})
}

func TestCustomersByToken(t *testing.T) {
	c := NewCustomers(newFakeUsers(t), time.Minute)
	ctx := context.Background()
	if u, err := c.ByToken(ctx, "tok-alice"); err != nil || u.Username != "alice" {
		t.Errorf("token = %v %v", u, err)
	}
	for _, bad := range []string{"", "tok-bob", "TOK-ALICE"} {
		if _, err := c.ByToken(ctx, bad); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("token %q: %v", bad, err)
		}
	}
}

func TestCustomersConcurrent(t *testing.T) {
	c := NewCustomers(newFakeUsers(t), time.Minute)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			pw := "alice-pw1"
			if i%3 == 0 {
				pw = "wrong-pw1"
			}
			_, err := c.ByPassword(context.Background(), "alice", pw)
			if (err == nil) != (pw == "alice-pw1") {
				t.Errorf("goroutine %d: err = %v", i, err)
			}
		})
	}
	wg.Wait()
}
