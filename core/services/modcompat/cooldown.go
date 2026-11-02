package modcompat

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// UpstreamCooldown stops calling a third-party API after it answered 429,
// until the time it asked for. Modrinth and Technic both limit per IP, and Core
// is one IP for every panel user: hammering on through a 429 is how a limit
// becomes a block.
//
// ponytail: per replica, in memory. Two replicas can each spend one request
// finding out; a shared Redis key if that ever matters.
type UpstreamCooldown struct {
	mu    sync.Mutex
	until time.Time
}

const (
	UpstreamCooldownDefault = 30 * time.Second
	UpstreamCooldownMax     = 5 * time.Minute
)

// Blocked reports how long calls are still held back.
func (c *UpstreamCooldown) Blocked(now time.Time) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Before(c.until) {
		return c.until.Sub(now), true
	}
	return 0, false
}

// Trip starts a cooldown from a 429 response: Retry-After, else Modrinth's
// X-Ratelimit-Reset (seconds until the window resets), else a default.
func (c *UpstreamCooldown) Trip(h http.Header, now time.Time) time.Duration {
	d := UpstreamCooldownDefault
	for _, name := range []string{"Retry-After", "X-Ratelimit-Reset"} {
		if s, err := strconv.Atoi(h.Get(name)); err == nil && s > 0 {
			d = time.Duration(s) * time.Second
			break
		}
	}
	if d > UpstreamCooldownMax {
		d = UpstreamCooldownMax
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if u := now.Add(d); u.After(c.until) {
		c.until = u
	}
	return d
}
