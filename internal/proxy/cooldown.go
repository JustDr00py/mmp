package proxy

import (
	"sync"
	"time"
)

// Cooldowns tracks per-stream failure backoff, shared by every session so a
// stream that just failed for one channel is skipped by all of them.
// Each consecutive failure doubles the cooldown, from base up to max.
type Cooldowns struct {
	base, max time.Duration

	mu sync.Mutex
	m  map[int64]*cooldown
}

type cooldown struct {
	until    time.Time
	failures int
}

// NewCooldowns builds a tracker with the given floor and ceiling.
func NewCooldowns(base, max time.Duration) *Cooldowns {
	return &Cooldowns{base: base, max: max, m: map[int64]*cooldown{}}
}

// Active reports whether streamID is cooling down at now.
func (c *Cooldowns) Active(streamID int64, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	cd, ok := c.m[streamID]
	return ok && now.Before(cd.until)
}

// Until returns when streamID's cooldown ends (zero if none recorded).
func (c *Cooldowns) Until(streamID int64) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cd, ok := c.m[streamID]; ok {
		return cd.until
	}
	return time.Time{}
}

// Fail records a failure at now and returns the cooldown applied:
// base, 2*base, 4*base, ... capped at max.
func (c *Cooldowns) Fail(streamID int64, now time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	cd, ok := c.m[streamID]
	if !ok {
		cd = &cooldown{}
		c.m[streamID] = cd
	}
	d := c.base
	for i := 0; i < cd.failures && d < c.max; i++ {
		d *= 2
	}
	d = min(d, c.max)
	cd.failures++
	cd.until = now.Add(d)
	return d
}

// Reset forgets streamID's failure history, e.g. once it has streamed
// stably again or the prober saw it recover.
func (c *Cooldowns) Reset(streamID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, streamID)
}
