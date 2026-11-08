package gateway

import (
	"strings"
	"sync"
	"time"

	"dylaris-core/store"
)

// linkKeyCacheTTL bounds how stale a cached key row or cut-off answer can be.
// It is deliberately inside the edge's 30 s recheck of link:<token>, so a
// revoke or suspension handled by ANOTHER replica still reaches the tunnel no
// later than it did before the cache existed, plus at most this much.
const linkKeyCacheTTL = 10 * time.Second

type keyEntry struct {
	key     store.WarpAPIKey
	expires time.Time
}

type cutEntry struct {
	cut     bool
	expires time.Time
}

// linkKeyCache keeps the route-only link API from paying ~4 DB reads per
// heartbeat (a link beats every 5 s). Zero value is ready to use.
//
// ponytail: per replica and in-process, no cross-replica invalidation. Local
// revoke/roll drops entries at once; anything another replica does is picked up
// within linkKeyCacheTTL. Add pub/sub invalidation only if 10 s ever matters.
//
// Only successful loads are cached: a store error or not-found must never pin
// a refusal or an acceptance.
//
// What the bound means in practice:
//   - Read-only link endpoints (edges, voice, targets) can serve a revoked kit
//     for up to linkKeyCacheTTL on a replica that did not handle the revoke.
//     Writes stay guarded by LinkHeartbeat's linkRevoked re-reads, which are
//     deliberately NOT cached.
//   - A load racing a local drop can reinsert a pre-revoke row for up to the
//     TTL; same bound, same write guard.
//   - Suspension relies on the TTL alone: dropOwner has no production caller,
//     the suspend writers live outside this package.
type linkKeyCache struct {
	mu        sync.Mutex
	now       func() time.Time // test seam; nil means time.Now
	keys      map[string]keyEntry
	owners    map[string]cutEntry
	routingTo time.Time // gateway routing known ON until this instant
}

// routing reports GatewayRoutingEnabled, caching only a true answer: that
// helper swallows store errors into false, and a fault must not pin "disabled".
func (c *linkKeyCache) routing(load func() bool) bool {
	now := c.clock()
	c.mu.Lock()
	on := now.Before(c.routingTo)
	c.mu.Unlock()
	if on {
		return true
	}
	if !load() {
		return false
	}
	c.mu.Lock()
	c.routingTo = now.Add(linkKeyCacheTTL)
	c.mu.Unlock()
	return true
}

func (c *linkKeyCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// getKey returns the key row for a presented hash. Only route-only `link-`
// kits are cached; BYON and in-cluster warp keys always hit the store, so their
// enroll path keeps its exact revoke and roll-grace semantics.
//
// A key presented by its previous hash (store.WarpKeyRollGrace) is cached under
// that presented hash, so its entry dies within the TTL of the grace ending.
func (c *linkKeyCache) getKey(hash string, load func() (store.WarpAPIKey, error)) (store.WarpAPIKey, error) {
	now := c.clock()
	c.mu.Lock()
	if e, ok := c.keys[hash]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.key, nil
	}
	c.mu.Unlock()

	k, err := load()
	if err != nil || !strings.HasPrefix(k.NodeID, "link-") {
		return k, err
	}
	c.mu.Lock()
	if c.keys == nil {
		c.keys = map[string]keyEntry{}
	}
	// ponytail: O(n) sweep once per 1024 entries; fine to ~1000 active links
	// per replica, switch to a janitor or LRU beyond that.
	if len(c.keys) >= 1024 {
		for h, e := range c.keys {
			if !now.Before(e.expires) {
				delete(c.keys, h)
			}
		}
	}
	c.keys[hash] = keyEntry{key: k, expires: now.Add(linkKeyCacheTTL)}
	c.mu.Unlock()
	return k, nil
}

// cutOff returns whether the owner is past a billing grace.
func (c *linkKeyCache) cutOff(ownerID string, load func() (bool, error)) (bool, error) {
	now := c.clock()
	c.mu.Lock()
	if e, ok := c.owners[ownerID]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.cut, nil
	}
	c.mu.Unlock()

	cut, err := load()
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	if c.owners == nil {
		c.owners = map[string]cutEntry{}
	}
	if len(c.owners) >= 1024 {
		for o, e := range c.owners {
			if !now.Before(e.expires) {
				delete(c.owners, o)
			}
		}
	}
	c.owners[ownerID] = cutEntry{cut: cut, expires: now.Add(linkKeyCacheTTL)}
	c.mu.Unlock()
	return cut, nil
}

func (c *linkKeyCache) dropKey(hash string) {
	c.mu.Lock()
	delete(c.keys, hash)
	c.mu.Unlock()
}

// dropKeyID drops every entry for one key row. A rolled key can be cached under
// its previous hash, which the caller no longer knows, hence the scan (the map
// holds one entry per active link).
func (c *linkKeyCache) dropKeyID(id int) {
	c.mu.Lock()
	for h, e := range c.keys {
		if e.key.ID == id {
			delete(c.keys, h)
		}
	}
	c.mu.Unlock()
}

func (c *linkKeyCache) dropOwner(ownerID string) {
	c.mu.Lock()
	delete(c.owners, ownerID)
	c.mu.Unlock()
}
