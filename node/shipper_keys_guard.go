package main

import (
	"context"
	"log"
	"os"
	"time"

	"dylaris-pkg/validate"

	"github.com/redis/go-redis/v9"
)

// Bounding the keys a server's own log-shipper credential can write.
//
// That credential lives in the tenant's container, and the Redis ACL can scope
// it to six keys but cannot cap their size. Nothing the shipper itself writes
// is large (it trims the console stream and caps a line), but code that got
// hold of the credential could APPEND or XADD until the SHARED Redis is full -
// and the keys had no TTL, so maxmemory eviction could never take them back.
// The log-shipper no longer hands the credential to the server process; this
// is the second layer, for whatever still writes there: every pass, any of
// these keys over its cap is deleted, and a small one without a TTL is given one.
//
// Keys only, no SCAN (the node's ACL has none): the console streams of
// sub-servers are found through the sub-server directories, so a stream under
// a name with no directory is not visited. The credential is the real fix.

const (
	shipperGuardInterval = 30 * time.Second
	// The console stream holds at most 5000 lines of at most 64 KiB; a real one
	// is a few MiB. 64 MiB is far past anything the shipper writes.
	shipperStreamCap = 64 << 20
	// Everything else is a number, a flag or a short queue of commands.
	shipperSmallCap = 64 << 10
)

// ttl 0 means size cap only. The console streams get no TTL on purpose: a
// stopped server's console history stays as long as it always did (there is no
// retention setting for it), and their size is what is bounded.
type guardedKey struct {
	key string
	cap int64
	ttl time.Duration
}

// shipperGuardedKeys lists one server's keys: the six its credential may write
// (redisacl.BuildShipperACLRules), with the stream named once per sub-server.
func shipperGuardedKeys(uuid string, subServers []string) []guardedKey {
	p := "dylaris:server:" + uuid + ":"
	ks := []guardedKey{
		{p + "logs", shipperStreamCap, 0},
		{p + "java-heap", shipperSmallCap, time.Hour},
		{p + "input", shipperSmallCap, 24 * time.Hour},
		{p + "log_filter_rcon", shipperSmallCap, time.Hour},
		{p + "stop-requested", shipperSmallCap, time.Hour},
	}
	for _, s := range subServers {
		ks = append(ks, guardedKey{p + "logs:" + s, shipperStreamCap, 0})
	}
	return ks
}

// guardKey enforces one key's cap and TTL. Returns true when it was deleted.
func guardKey(ctx context.Context, rdb *redis.Client, k guardedKey) bool {
	used, err := rdb.MemoryUsage(ctx, k.key).Result()
	if err != nil {
		return false // missing (redis.Nil) or unreadable: nothing to do
	}
	if used > k.cap {
		if rdb.Del(ctx, k.key).Err() == nil {
			log.Printf("shipper-guard: deleted %s (%d bytes, cap %d)", k.key, used, k.cap)
			return true
		}
		return false
	}
	if k.ttl == 0 {
		return false
	}
	// -1 is "no TTL". A key that has one keeps it: the shipper and Core set
	// their own, shorter ones.
	if ttl, terr := rdb.TTL(ctx, k.key).Result(); terr == nil && ttl == -1 {
		rdb.Expire(ctx, k.key, k.ttl)
	}
	return false
}

// shipperGuardMaxSubServers bounds the sub-server streams one server costs a
// pass. The directory listing is the tenant's /data, so without a bound plugin
// code could mkdir its way to one Redis round trip per inode, every 30s,
// delaying the guard for every other server on the node.
// ponytail: a server with more sub-servers than this has its inactive streams
// skipped; they are written only while active, and the active one is first.
const shipperGuardMaxSubServers = 64

// shipperGuardSubServers lists the sub-server names whose console stream the
// guard visits: the active one first, then other directories that are valid
// sub-server names, at most shipperGuardMaxSubServers in all.
func shipperGuardSubServers(serverDir string) []string {
	var subs []string
	active := readActiveSubServer(serverDir)
	if active != "" {
		subs = append(subs, active)
	}
	entries, err := os.ReadDir(serverDir)
	if err != nil {
		return subs
	}
	for _, e := range entries {
		if len(subs) >= shipperGuardMaxSubServers {
			break
		}
		if e.IsDir() && e.Name() != active && validate.IsSubServerName(e.Name()) {
			subs = append(subs, e.Name())
		}
	}
	return subs
}

// guardShipperKeys runs one pass over every running server on this node.
func (dm *DockerManager) guardShipperKeys(ctx context.Context, rdb *redis.Client) {
	running, err := dm.ListRunningMCContainers()
	if err != nil {
		return
	}
	for _, c := range running {
		for _, k := range shipperGuardedKeys(c.UUID, shipperGuardSubServers(dm.resolveLocalServerPath(c.UUID))) {
			guardKey(ctx, rdb, k)
		}
	}
}

func startShipperKeyGuard(ctx context.Context, dm *DockerManager, rdb *redis.Client) {
	if dm == nil || rdb == nil {
		return
	}
	go func() {
		t := time.NewTicker(shipperGuardInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				dm.guardShipperKeys(ctx, rdb)
			}
		}
	}()
}
