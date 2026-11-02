package services

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Where cached third-party metadata lives.
//
// The Modrinth proxy and the cross-version availability check both cache public
// Modrinth responses. By default those go into the Redis Core already has, which
// is the same instance that carries the node command streams, the per-node ACL
// users and every server's live status.
//
// That default is the right one for almost everyone: Redis is not optional here,
// Core cannot boot without it, and the cached data is public and reconstructible.
// What it is NOT is free of consequence, because the instance runs with no
// maxmemory: one measured Modrinth version list runs 290 KB to 1.2 MB, and the
// proxy keys them per filter combination for an hour. Enough browsing and the
// cache is competing for memory with the command bus.
//
// Two answers, and the cheap one comes first:
//
//  1. cap what is cached at all (see maxCachedResponseBytes in the proxy), which
//     removes the megabyte tail measured above.
//  2. for an operator who wants the cache off the control plane entirely, point
//     it at a Redis of its own.
//
// (2) is OPTIONAL on purpose. Making it a gate before modpacks or mods could be
// used would gate a feature behind something the whole system already requires,
// so the gate could never fail and its only correct answer would be "the one you
// already have".

// CacheConfig is a dedicated cache Redis. A blank Addr means "use the Redis Core
// already has".
type CacheConfig struct {
	Addr     string
	Username string
	Password string
	DB       int
	TLS      bool
}

// Configured reports whether a dedicated cache endpoint is set.
func (c CacheConfig) Configured() bool { return strings.TrimSpace(c.Addr) != "" }

// Cache is the small Redis surface the caches need. Every operation is
// best-effort: a cache that errors is a cache miss, never a failed request.
type Cache struct {
	mu sync.RWMutex
	// bus is the Redis Core uses for everything else. It is the default target
	// and the one used when no dedicated endpoint is configured.
	bus *redis.Client
	// dedicated is the operator-configured cache endpoint, nil when unset.
	dedicated *redis.Client
	cfg       CacheConfig
}

func NewCache(bus *redis.Client) *Cache { return &Cache{bus: bus} }

// Reconfigure points the cache at a dedicated endpoint, or back at the bus when
// cfg carries no address. It dials and pings before swapping, so a bad address
// is reported to the caller instead of silently disabling the cache.
//
// A failed ping changes NOTHING: this is the save path, and refusing to store an
// endpoint that does not answer is the whole point of it. Boot is a different
// question - see Adopt.
func (c *Cache) Reconfigure(ctx context.Context, cfg CacheConfig) error {
	return c.point(ctx, cfg, false)
}

// Adopt points the cache at a configuration that is ALREADY STORED, whether or
// not the endpoint answers right now. It is the boot path.
//
// The difference from Reconfigure is the whole reason it exists. Reconfigure
// leaves state untouched when the ping fails, which is right when an operator is
// typing an address - and wrong at boot, where it silently left cfg unset and so
// sent every cached Modrinth response to the control-plane Redis. That is exactly
// what an operator who configured a dedicated cache moved it away from, and the
// contract in client() below promises it does not happen. It held after a save
// and not after a restart.
//
// It was also invisible: the settings screen renders the address from the
// SETTINGS while Status() reports on c.cfg, so the page showed the dedicated host
// next to a green healthy tick that had pinged the bus.
//
// The client is kept even when the ping fails. go-redis dials lazily per command,
// so an endpoint that was down at boot starts working on its own once it is back,
// and the caches degrade to misses in the meantime - which is what they already
// do for an endpoint that dies after a successful configure. One behaviour, not
// two.
func (c *Cache) Adopt(ctx context.Context, cfg CacheConfig) error {
	return c.point(ctx, cfg, true)
}

func (c *Cache) point(ctx context.Context, cfg CacheConfig, keepUnreachable bool) error {
	var next *redis.Client
	var pingErr error
	if cfg.Configured() {
		opts := &redis.Options{
			Addr:     strings.TrimSpace(cfg.Addr),
			Username: cfg.Username,
			Password: cfg.Password,
			DB:       cfg.DB,
			// Tight and deliberate. A cache is never a request's dependency, so
			// an endpoint that stops answering has to cost a miss, not a stall:
			// with the library defaults a dead endpoint held each call for
			// seconds, which would have shown up as a panel that hangs rather
			// than one that got slower.
			DialTimeout:  2 * time.Second,
			ReadTimeout:  2 * time.Second,
			WriteTimeout: 2 * time.Second,
			MaxRetries:   1,
		}
		if cfg.TLS {
			opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		next = redis.NewClient(opts)
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := next.Ping(pingCtx).Err(); err != nil {
			pingErr = fmt.Errorf("cache redis at %s: %w", cfg.Addr, err)
			if !keepUnreachable {
				_ = next.Close()
				return pingErr
			}
		}
	}

	c.mu.Lock()
	old := c.dedicated
	c.dedicated = next
	c.cfg = cfg
	c.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	// Adopted, and still reported: the caller logs it, and Status() now says
	// "dedicated, not answering" instead of describing the bus.
	return pingErr
}

// client returns the endpoint to use, or nil when there is none.
//
// A configured-but-unreachable dedicated endpoint deliberately does NOT fall
// back to the bus: the operator moved the cache off the control plane on
// purpose, and quietly putting it back would undo that without saying so. The
// caches degrade to "no cache" instead, which costs latency and nothing else.
func (c *Cache) client() *redis.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cfg.Configured() {
		return c.dedicated
	}
	return c.bus
}

// Status describes where the cache is pointing, for the settings screen.
type CacheStatus struct {
	Dedicated bool   `json:"dedicated"`
	Addr      string `json:"addr,omitempty"`
	Healthy   bool   `json:"healthy"`
	Error     string `json:"error,omitempty"`
}

func (c *Cache) Status(ctx context.Context) CacheStatus {
	c.mu.RLock()
	cfg := c.cfg
	client := c.dedicated
	if !cfg.Configured() {
		client = c.bus
	}
	c.mu.RUnlock()

	st := CacheStatus{Dedicated: cfg.Configured(), Addr: cfg.Addr}
	if client == nil {
		st.Error = "not connected"
		return st
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		st.Error = err.Error()
		return st
	}
	st.Healthy = true
	return st
}

func (c *Cache) Get(ctx context.Context, key string) (string, bool) {
	client := c.client()
	if client == nil {
		return "", false
	}
	v, err := client.Get(ctx, key).Result()
	if err != nil || v == "" {
		return "", false
	}
	return v, true
}

func (c *Cache) Set(ctx context.Context, key, value string, ttl time.Duration) {
	client := c.client()
	if client == nil {
		return
	}
	client.Set(ctx, key, value, ttl)
}

// GetMany returns one entry per key, "" where the cache had nothing. It never
// returns a short slice, so callers can index it against their key list.
func (c *Cache) GetMany(ctx context.Context, keys []string) []string {
	out := make([]string, len(keys))
	client := c.client()
	if client == nil || len(keys) == 0 {
		return out
	}
	vals, err := client.MGet(ctx, keys...).Result()
	if err != nil {
		return out
	}
	for i := range vals {
		if i >= len(out) {
			break
		}
		if s, ok := vals[i].(string); ok {
			out[i] = s
		}
	}
	return out
}

func (c *Cache) SetMany(ctx context.Context, values map[string]string, ttl time.Duration) {
	client := c.client()
	if client == nil || len(values) == 0 {
		return
	}
	pipe := client.Pipeline()
	for k, v := range values {
		pipe.Set(ctx, k, v, ttl)
	}
	_, _ = pipe.Exec(ctx)
}

// Close releases a dedicated connection. The bus client is owned elsewhere and
// is deliberately left alone.
func (c *Cache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dedicated != nil {
		_ = c.dedicated.Close()
		c.dedicated = nil
	}
}

// MaxCachedResponseBytes caps what one cache entry may weigh.
//
// Measured against the live API: a project's version list runs 290 KB for
// Sodium filtered to one loader, 494 KB unfiltered, and 1.19 MB for Fabric API.
// The proxy keys those per filter combination for an hour and the shipped Redis
// runs with no maxmemory, so without a cap the tail of that distribution is what
// decides how much memory the control plane has left. Over the cap the response
// is served straight through: one extra upstream call, no stored megabyte.
const MaxCachedResponseBytes = 512 << 10
