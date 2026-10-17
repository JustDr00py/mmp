package auth

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func newTestLimiter(max int, lockout time.Duration) (*Limiter, *fakeClock) {
	l := NewLimiter(max, lockout)
	c := &fakeClock{t: time.Unix(1700000000, 0)}
	l.now = c.Now
	return l, c
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

func TestLimiterLocksAfterMaxAttempts(t *testing.T) {
	l, _ := newTestLimiter(3, 5*time.Minute)
	for range 2 {
		if ok, _ := l.allowed("ip:1.2.3.4"); !ok {
			t.Fatal("locked too early")
		}
		l.record(false, "ip:1.2.3.4")
	}
	if ok, _ := l.allowed("ip:1.2.3.4"); !ok {
		t.Fatal("locked before the third failure")
	}
	l.record(false, "ip:1.2.3.4")
	ok, retry := l.allowed("ip:1.2.3.4")
	if ok || retry != 5*time.Minute {
		t.Fatalf("want lockout with 5m left, got ok=%v retry=%v", ok, retry)
	}
	// The lock applies per key: another address is unaffected.
	if ok, _ := l.allowed("ip:5.6.7.8"); !ok {
		t.Fatal("lock leaked across keys")
	}
}

func TestLimiterUnlockAfterTTL(t *testing.T) {
	l, c := newTestLimiter(2, 5*time.Minute)
	l.record(false, "u")
	l.record(false, "u")
	if ok, _ := l.allowed("u"); ok {
		t.Fatal("should be locked")
	}
	c.t = c.t.Add(5*time.Minute + time.Second)
	if ok, _ := l.allowed("u"); !ok {
		t.Fatal("lock did not expire")
	}
	// After expiry the counter restarted: one more failure must not relock.
	l.record(false, "u")
	if ok, _ := l.allowed("u"); !ok {
		t.Fatal("relocked after a single post-expiry failure")
	}
}

func TestLimiterSuccessResets(t *testing.T) {
	l, _ := newTestLimiter(3, 5*time.Minute)
	l.record(false, "u", "ip:x")
	l.record(false, "u", "ip:x")
	l.record(true, "u", "ip:x") // a correct password clears both keys
	l.record(false, "u", "ip:x")
	if ok, _ := l.allowed("u"); !ok {
		t.Fatal("success did not reset the counter")
	}
}

func TestLimiterSlowTrickleDoesNotAccumulate(t *testing.T) {
	l, c := newTestLimiter(3, time.Minute)
	for range 5 {
		l.record(false, "u")
		c.t = c.t.Add(2 * time.Minute) // gaps larger than the lockout
	}
	if ok, _ := l.allowed("u"); !ok {
		t.Fatal("sparse failures accumulated into a lockout")
	}
}

func TestLimiterDisabled(t *testing.T) {
	var nilLimiter *Limiter
	if ok, retry := nilLimiter.Allow(Attempt{user: "u"}); !ok || retry != 0 {
		t.Fatal("nil limiter must be a no-op")
	}
	nilLimiter.Done(Attempt{user: "u"}, false) // must not panic
	l := NewLimiter(0, time.Minute)
	for range 3 {
		l.Done(Attempt{user: "u"}, false)
	}
	if ok, _ := l.Allow(Attempt{user: "u"}); !ok {
		t.Fatal("maxAttempts <= 0 must disable limiting")
	}
}

func TestLimiterBounded(t *testing.T) {
	l, _ := newTestLimiter(2, time.Hour)
	for i := range maxLimiterEntries + 10 {
		l.record(false, "k"+strconv.Itoa(i))
	}
	l.mu.Lock()
	n := len(l.seen)
	l.mu.Unlock()
	if n > maxLimiterEntries {
		t.Fatalf("table grew to %d entries", n)
	}
}

func TestLimiterBoundedKeepsLocks(t *testing.T) {
	l, _ := newTestLimiter(1, time.Hour)
	for i := range maxLimiterEntries + 10 {
		l.record(false, "k"+strconv.Itoa(i)) // every key locks at once (max 1)
	}
	// Flooding evicts the locks nearest their end, a tenth at a time; it
	// never wipes the table.
	l.mu.Lock()
	n := len(l.seen)
	l.mu.Unlock()
	if n > maxLimiterEntries || n < maxLimiterEntries*8/10 {
		t.Fatalf("table holds %d entries, want at most %d and most of it kept", n, maxLimiterEntries)
	}
	if ok, _ := l.allowed("k" + strconv.Itoa(maxLimiterEntries+9)); ok {
		t.Fatal("the newest lock was dropped")
	}

	// Unlocked counters go before any lock.
	l, _ = newTestLimiter(2, time.Hour)
	l.record(false, "locked")
	l.record(false, "locked")
	for i := range maxLimiterEntries + 10 {
		l.record(false, "k"+strconv.Itoa(i)) // one failure each: counting, not locked
	}
	if ok, _ := l.allowed("locked"); ok {
		t.Fatal("a lock was evicted while unlocked counters remained")
	}
}

func TestLimiterKnownNetworkSurvivesUserLockout(t *testing.T) {
	l, c := newTestLimiter(3, 5*time.Minute)
	home := Attempt{network: "203.0.113.7", user: "alice"}
	l.Done(home, true) // alice's player logs in from home
	for i := range 3 { // a stranger guesses from three networks
		l.Done(Attempt{network: "198.51.100." + strconv.Itoa(i), user: "alice"}, false)
	}
	if ok, _ := l.Allow(Attempt{network: "198.51.100.9", user: "alice"}); ok {
		t.Fatal("username not locked for other networks")
	}
	if ok, _ := l.Allow(home); !ok {
		t.Fatal("the owner's network was locked out")
	}
	// The owner's successes do not hand the stranger fresh guesses.
	l.Done(home, true)
	if ok, _ := l.Allow(Attempt{network: "198.51.100.9", user: "alice"}); ok {
		t.Fatal("a success from home lifted the username lock elsewhere")
	}
	// A network vouches for knownFor only.
	c.t = c.t.Add(knownFor)
	l.Done(Attempt{network: "198.51.100.1", user: "alice"}, false)
	l.Done(Attempt{network: "198.51.100.2", user: "alice"}, false)
	l.Done(Attempt{network: "198.51.100.3", user: "alice"}, false)
	if ok, _ := l.Allow(home); ok {
		t.Fatal("an expired known network still bypasses the lock")
	}
	// The network's own failures still lock it, known or not.
	l, _ = newTestLimiter(3, 5*time.Minute)
	l.Done(home, true)
	for range 3 {
		l.Done(home, false)
	}
	if ok, _ := l.Allow(home); ok {
		t.Fatal("failures from a known network did not lock it")
	}
}

func TestClientNetwork(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/get.php", nil)
	for addr, want := range map[string]string{
		"10.0.0.7:51234":                     "10.0.0.7",
		"[::ffff:10.0.0.7]:80":               "10.0.0.7",
		"[2001:db8:1:2:aaaa:bbbb:cccc:1]:80": "2001:db8:1:2::/64",
		"[2001:db8:1:2::99]:80":              "2001:db8:1:2::/64",
		"unix":                               "unix",
	} {
		r.RemoteAddr = addr
		if got := ClientNetwork(r); got != want {
			t.Errorf("ClientNetwork(%s) = %q, want %q", addr, got, want)
		}
	}
	r.RemoteAddr = "10.0.0.7:51234"
	a := NewAttempt(r, "alice")
	if a.userKey() != "user:alice" || a.networkKey() != "ip:10.0.0.7" {
		t.Errorf("attempt keys = %s %s", a.userKey(), a.networkKey())
	}
	r.RemoteAddr = "[::1]:80"
	if got := RemoteIP(r); got != "::1" {
		t.Fatalf("ipv6: got %q", got)
	}
}
