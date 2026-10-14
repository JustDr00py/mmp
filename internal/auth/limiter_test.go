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
		if ok, _ := l.Allowed("ip:1.2.3.4"); !ok {
			t.Fatal("locked too early")
		}
		l.Record(false, "ip:1.2.3.4")
	}
	if ok, _ := l.Allowed("ip:1.2.3.4"); !ok {
		t.Fatal("locked before the third failure")
	}
	l.Record(false, "ip:1.2.3.4")
	ok, retry := l.Allowed("ip:1.2.3.4")
	if ok || retry != 5*time.Minute {
		t.Fatalf("want lockout with 5m left, got ok=%v retry=%v", ok, retry)
	}
	// The lock applies per key: another address is unaffected.
	if ok, _ := l.Allowed("ip:5.6.7.8"); !ok {
		t.Fatal("lock leaked across keys")
	}
}

func TestLimiterUnlockAfterTTL(t *testing.T) {
	l, c := newTestLimiter(2, 5*time.Minute)
	l.Record(false, "u")
	l.Record(false, "u")
	if ok, _ := l.Allowed("u"); ok {
		t.Fatal("should be locked")
	}
	c.t = c.t.Add(5*time.Minute + time.Second)
	if ok, _ := l.Allowed("u"); !ok {
		t.Fatal("lock did not expire")
	}
	// After expiry the counter restarted: one more failure must not relock.
	l.Record(false, "u")
	if ok, _ := l.Allowed("u"); !ok {
		t.Fatal("relocked after a single post-expiry failure")
	}
}

func TestLimiterSuccessResets(t *testing.T) {
	l, _ := newTestLimiter(3, 5*time.Minute)
	l.Record(false, "u", "ip:x")
	l.Record(false, "u", "ip:x")
	l.Record(true, "u", "ip:x") // a correct password clears both keys
	l.Record(false, "u", "ip:x")
	if ok, _ := l.Allowed("u"); !ok {
		t.Fatal("success did not reset the counter")
	}
}

func TestLimiterSlowTrickleDoesNotAccumulate(t *testing.T) {
	l, c := newTestLimiter(3, time.Minute)
	for range 5 {
		l.Record(false, "u")
		c.t = c.t.Add(2 * time.Minute) // gaps larger than the lockout
	}
	if ok, _ := l.Allowed("u"); !ok {
		t.Fatal("sparse failures accumulated into a lockout")
	}
}

func TestLimiterDisabled(t *testing.T) {
	var nilLimiter *Limiter
	if ok, retry := nilLimiter.Allowed("u"); !ok || retry != 0 {
		t.Fatal("nil limiter must be a no-op")
	}
	nilLimiter.Record(false, "u") // must not panic
	l := NewLimiter(0, time.Minute)
	l.Record(false, "u")
	if ok, _ := l.Allowed("u"); !ok {
		t.Fatal("maxAttempts <= 0 must disable limiting")
	}
}

func TestLimiterBounded(t *testing.T) {
	l, _ := newTestLimiter(2, time.Hour)
	for i := range maxLimiterEntries + 10 {
		l.Record(false, "k"+strconv.Itoa(i))
	}
	l.mu.Lock()
	n := len(l.seen)
	l.mu.Unlock()
	if n > maxLimiterEntries {
		t.Fatalf("table grew to %d entries", n)
	}
}

func TestAttemptKeys(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/get.php", nil)
	r.RemoteAddr = "10.0.0.7:51234"
	keys := AttemptKeys(r, "alice")
	want := []string{"ip:10.0.0.7", "user:alice"}
	if keys[0] != want[0] || keys[1] != want[1] {
		t.Fatalf("got %v want %v", keys, want)
	}
	// IPv6 and portless RemoteAddr both survive.
	r.RemoteAddr = "[::1]:80"
	if got := RemoteIP(r); got != "::1" {
		t.Fatalf("ipv6: got %q", got)
	}
	r.RemoteAddr = "unix"
	if got := RemoteIP(r); got != "unix" {
		t.Fatalf("portless: got %q", got)
	}
}
