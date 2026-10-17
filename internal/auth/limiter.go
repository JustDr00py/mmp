package auth

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// maxLimiterEntries bounds the failure table (like the bcrypt cache):
// random usernames or many client addresses cannot grow it without end.
// When full, stale entries go first, then those that matter least; locks
// are never wiped wholesale, so flooding the table does not unlock anyone.
const maxLimiterEntries = 10000

// knownFor is how long a successful login vouches for its client network:
// from there, that username's lockout does not apply.
const knownFor = 30 * 24 * time.Hour

// Limiter dampens brute-force password attempts in memory. An attempt is
// counted against its client network (an IPv4 address or an IPv6 /64) and
// its username; either key that fails maxAttempts times within lockout is
// refused for lockout. A username lockout does not shut out networks the
// user recently logged in from, so a stranger who knows a username cannot
// lock its owner out. A success clears its network's counter but not the
// username's: logins from the owner must not hand an attacker on other
// networks fresh guesses. It is single-process by design and intentionally
// cheap — the real password check stays the bcrypt compare. A nil Limiter,
// or one built with maxAttempts <= 0, is a no-op.
type Limiter struct {
	maxAttempts int
	lockout     time.Duration
	now         func() time.Time // injectable clock

	mu    sync.Mutex
	seen  map[string]*attempt
	known map[string]time.Time // username + network -> last success
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
		now: time.Now, seen: make(map[string]*attempt), known: make(map[string]time.Time)}
}

// Attempt is one password attempt: who it claims to be and where from.
type Attempt struct {
	network, user string
}

// NewAttempt describes a password attempt on r for username.
func NewAttempt(r *http.Request, username string) Attempt {
	return Attempt{network: ClientNetwork(r), user: username}
}

func (a Attempt) networkKey() string { return "ip:" + a.network }
func (a Attempt) userKey() string    { return "user:" + a.user }
func (a Attempt) knownKey() string   { return a.user + "\x00" + a.network }

// Allow reports whether attempt a may proceed. When refused, it returns
// how long the lockout still runs.
func (l *Limiter) Allow(a Attempt) (bool, time.Duration) {
	if l.disabled() {
		return true, 0
	}
	keys := []string{a.networkKey()}
	if !l.isKnown(a) {
		keys = append(keys, a.userKey())
	}
	return l.allowed(keys...)
}

// Done records the outcome of attempt a. Infrastructure errors should not
// be recorded at all.
func (l *Limiter) Done(a Attempt, ok bool) {
	if l.disabled() {
		return
	}
	if !ok {
		l.record(false, a.networkKey(), a.userKey())
		return
	}
	l.record(true, a.networkKey())
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.known[a.knownKey()]; !ok && len(l.known) >= maxLimiterEntries {
		l.forgetKnownLocked(now)
	}
	l.known[a.knownKey()] = now
}

func (l *Limiter) isKnown(a Attempt) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.known[a.knownKey()]
	return ok && l.now().Sub(at) < knownFor
}

// forgetKnownLocked makes room in the known table: expired entries, or
// else the oldest one.
func (l *Limiter) forgetKnownLocked(now time.Time) {
	var oldest string
	var oldestAt time.Time
	for k, at := range l.known {
		if now.Sub(at) >= knownFor {
			delete(l.known, k)
			continue
		}
		if oldest == "" || at.Before(oldestAt) {
			oldest, oldestAt = k, at
		}
	}
	if len(l.known) >= maxLimiterEntries {
		delete(l.known, oldest)
	}
}

// allowed reports whether an attempt counted under all of keys may
// proceed, and how long the longest remaining lockout still runs.
func (l *Limiter) allowed(keys ...string) (bool, time.Duration) {
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

// record folds an attempt's outcome into every key: a failure bumps each
// counter (locking each key independently at the limit), a success clears
// them.
func (l *Limiter) record(ok bool, keys ...string) {
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
				l.evictLocked(now)
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

// evictLocked makes room in the failure table: entries neither locked nor
// counting any more, or else the tenth that matters least (unlocked
// counters before any lock; the oldest failure, or the lock ending first).
// Evicting a batch keeps a flood of new keys from rescanning the table on
// every failure.
func (l *Limiter) evictLocked(now time.Time) {
	type entry struct {
		key    string
		locked bool
		at     time.Time
	}
	var live []entry
	for k, a := range l.seen {
		locked := a.lockedTo.After(now)
		switch {
		case !locked && now.Sub(a.lastFail) > l.lockout:
			delete(l.seen, k)
		case locked:
			live = append(live, entry{k, true, a.lockedTo})
		default:
			live = append(live, entry{k, false, a.lastFail})
		}
	}
	if len(l.seen) < maxLimiterEntries {
		return
	}
	slices.SortFunc(live, func(a, b entry) int {
		if a.locked != b.locked {
			if a.locked {
				return 1
			}
			return -1
		}
		return a.at.Compare(b.at)
	})
	for _, e := range live[:max(1, len(live)/10)] {
		delete(l.seen, e.key)
	}
}

func (l *Limiter) disabled() bool { return l == nil || l.maxAttempts <= 0 }

// LockedError reports a refused attempt and how long the caller should wait.
type LockedError struct {
	RetryAfter time.Duration
}

func (e *LockedError) Error() string { return "too many attempts" }

// ClientNetwork is the client's address as the limiter and HLS leases
// count it: an IPv4 address, or the /64 of an IPv6 one (a single home or
// phone usually holds a whole /64 and may rotate through it).
func ClientNetwork(r *http.Request) string {
	ip := RemoteIP(r)
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if a = a.Unmap(); a.Is6() {
		return netip.PrefixFrom(a, 64).Masked().String()
	}
	return a.String()
}

// RemoteIP strips the port from RemoteAddr (test servers use pipes).
func RemoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
