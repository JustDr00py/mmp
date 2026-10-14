package auth

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// maxLimiterEntries bounds the failure table (like the bcrypt cache):
// random usernames or spoofed IPs cannot grow it without end. When full it
// is reset wholesale — the oldest locks are near expiry by then anyway.
const maxLimiterEntries = 10000

// Limiter dampens brute-force password attempts in memory: a key (client IP
// or username) that fails maxAttempts times within lockout is refused for
// lockout; a success clears its counter. It is single-process by design and
// intentionally cheap — the real password check stays the bcrypt compare.
// A nil Limiter, or one built with maxAttempts <= 0, is a no-op.
type Limiter struct {
	maxAttempts int
	lockout     time.Duration
	now         func() time.Time // injectable clock

	mu   sync.Mutex
	seen map[string]*attempt
}

type attempt struct {
	fails    int       // failures since the last success or lock
	lastFail time.Time // most recent failure
	lockedTo time.Time // attempts are refused without a password check until then
}

// NewLimiter builds a Limiter that locks a key out for lockout after
// maxAttempts failed attempts. maxAttempts <= 0 disables it entirely.
func NewLimiter(maxAttempts int, lockout time.Duration) *Limiter {
	return &Limiter{maxAttempts: maxAttempts, lockout: lockout,
		now: time.Now, seen: make(map[string]*attempt)}
}

// Allowed reports whether an attempt from any of keys may proceed. When
// refused, it returns how long the longest remaining lockout still runs.
func (l *Limiter) Allowed(keys ...string) (bool, time.Duration) {
	if l.disabled() {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var wait time.Duration
	for _, k := range keys {
		if a := l.seen[k]; a != nil {
			if d := a.lockedTo.Sub(now); d > wait {
				wait = d
			}
		}
	}
	return wait <= 0, wait
}

// Record folds an attempt's outcome into every key: a failure bumps each
// counter (locking each key independently at the limit), a success clears
// them. Infrastructure errors should not be recorded at all.
func (l *Limiter) Record(ok bool, keys ...string) {
	if l.disabled() {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if ok {
			delete(l.seen, k)
			continue
		}
		a := l.seen[k]
		if a == nil {
			if len(l.seen) >= maxLimiterEntries {
				l.seen = make(map[string]*attempt)
			}
			a = &attempt{}
			l.seen[k] = a
		}
		// Failures spaced further apart than the lockout start a new
		// count: a slow trickle must not accumulate into a lockout.
		if now.Sub(a.lastFail) > l.lockout {
			a.fails = 0
		}
		a.fails++
		a.lastFail = now
		if a.fails >= l.maxAttempts {
			a.lockedTo = now.Add(l.lockout)
			a.fails = 0 // after the lockout expires, counting restarts
		}
	}
}

func (l *Limiter) disabled() bool { return l == nil || l.maxAttempts <= 0 }

// LockedError reports a refused attempt and how long the caller should wait.
type LockedError struct {
	RetryAfter time.Duration
}

func (e *LockedError) Error() string { return "too many attempts" }

// AttemptKeys builds the limiter keys of a password attempt: the client IP
// (RemoteAddr; X-Forwarded-For is not trusted) and the identity. The IP key
// catches spraying across many accounts, the identity key catches a targeted
// attack from rotating IPs.
func AttemptKeys(r *http.Request, username string) []string {
	return []string{"ip:" + RemoteIP(r), "user:" + username}
}

// RemoteIP strips the port from RemoteAddr (test servers use pipes).
func RemoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
