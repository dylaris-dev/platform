package services

import (
	"context"
	"log"
	"strings"

	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
)

// RemoveDeletedServers removes what a deleted server leaves outside its own row:
// the gateway routes that point at it and its leftover Redis keys.
//
// ONE function rather than two the caller has to remember, because forgetting
// one of a pair is the defect this exists to fix: the single-server delete did
// both, the two node deletes did neither, and the first attempt at this fix
// added only the routes back.
//
// Best-effort throughout, and it must stay that way: every caller has already
// deleted the server rows when it gets here, so a Redis hiccup cannot be allowed
// to turn a completed delete into a failure. Returns how many routes matched.
func RemoveDeletedServers(ctx context.Context, gw GatewayProvider, rdb *redis.Client, uuids []string) int {
	matched := removeServerRoutes(ctx, gw, rdb, uuids)
	for _, u := range uuids {
		removeServerKeys(ctx, rdb, u)
	}
	return matched
}

// removeServerKeys drops the housekeeping keys of one deleted server.
//
// Most of them carry a TTL and expire on their own. Two do not: the log stream
// dylaris:server:<uuid>:logs[:<sub>] and the stats buffer are Redis STREAMS, and
// while each is length-capped, nothing ever removes them - so the COUNT of
// orphaned streams grew by two per deleted server, forever.
//
// Scanning by the uuid prefix rather than listing key names keeps this correct
// as keys are added, and covers one log stream per sub-server without having to
// know their names after the rows are gone. The prefix is exact, so the sweep
// cannot reach another server's keys.
//
// Core runs this rather than the node because the node's Redis ACL is scoped to
// the servers it currently owns - by delete time that grant is on its way out,
// and the node may be offline entirely.
func removeServerKeys(ctx context.Context, rdb *redis.Client, uuid string) {
	if rdb == nil || strings.TrimSpace(uuid) == "" {
		return
	}
	pattern := "dylaris:server:" + uuid + ":*"
	var cursor uint64
	removed := 0
	for {
		keys, next, err := rdb.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			log.Printf("delete server %s: scanning leftover Redis keys failed: %v", uuid, err)
			return
		}
		if len(keys) > 0 {
			if err := rdb.Del(ctx, keys...).Err(); err != nil {
				log.Printf("delete server %s: removing leftover Redis keys failed: %v", uuid, err)
				return
			}
			removed += len(keys)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	if removed > 0 {
		log.Printf("delete server %s: removed %d leftover Redis key(s)", uuid, removed)
	}
}

// RouteBelongsToServer answers whether one route points at one server.
//
// The single definition of that question, because there are two shapes and they
// are not interchangeable: routes written before `server_uuid` was persisted
// carry only `target_ip = "mc_<uuid>"`, newer ones carry both. A second, narrower
// copy of this check is how the per-server route delete came to answer "not
// found" for a route that is plainly the server's own.
//
// An empty uuid matches NOTHING. Without that guard it would match every managed
// route from before that column existed.
func RouteBelongsToServer(rt GatewayRoute, uuid string) bool {
	if uuid == "" {
		return false
	}
	return rt.ServerUUID == uuid || rt.TargetIP == "mc_"+uuid
}

// removeServerRoutes removes the gateway routes that point at the given servers.
//
// It exists because a route does not cascade with the server row. Routes live in
// Redis and in the hub's own database, so deleting servers from SQL removes what
// the panel lists and leaves the address answering - and in gateway routing that
// address is the only way in, so it keeps sending players to a target that is
// gone. Deleting the LAST server of a machine left the address pointing at
// nothing at all; one was still answering on production, with a Dylaris
// placeholder behind it, weeks after its node was deleted.
//
// Both layers are kept, for the reasons the single-server version documented:
// the queue message is the source of truth (the hub deletes its row, so its next
// sync does not republish the route), and the direct key drop is what takes the
// address out of traffic while the hub catches up - and the only thing that
// works at all when the hub's queue worker is wedged.
//
// Returns how many routes matched.
func removeServerRoutes(ctx context.Context, gw GatewayProvider, rdb *redis.Client, uuids []string) int {
	// By server first, and whatever the cache holds: the per-domain deletes
	// below can only name routes Redis still has, so a cache that lost its
	// route keys left the hub's rows standing and the next sync published the
	// addresses again, pointing at servers that no longer exist.
	if gw != nil {
		for _, u := range uuids {
			if err := gw.DeleteServerRoutes(u); err != nil {
				log.Printf("remove server routes: hub delete for server %s: %v", u, err)
			}
		}
	}
	if rdb == nil || len(uuids) == 0 {
		return 0
	}
	// One read for the whole set: a force-deleted node can hold dozens of
	// servers, and a route lookup per server is that many round trips.
	routes := GetRoutesFromRedis(ctx, rdb)
	matched := 0
	for _, rt := range routes {
		if !routeBelongsToAny(rt, uuids) {
			continue
		}
		matched++
		if gw != nil {
			if err := gw.DeleteRoute(rt.Domain); err != nil {
				log.Printf("remove server routes: gateway delete %s: %v", rt.Domain, err)
			}
		}
		pipe := rdb.Pipeline()
		pipe.Del(ctx, "route:"+rt.Domain)
		pipe.SRem(ctx, "sys:index:routes", rt.Domain)
		if _, err := pipe.Exec(ctx); err != nil {
			log.Printf("remove server routes: redis drop %s: %v", rt.Domain, err)
		}
	}
	return matched
}

func routeBelongsToAny(rt GatewayRoute, uuids []string) bool {
	for _, u := range uuids {
		if RouteBelongsToServer(rt, u) {
			return true
		}
	}
	return false
}

// ServerUUIDs is the uuid list of a set of servers, for the node-delete paths
// that read a machine's servers before removing them.
func ServerUUIDs(servers []models.Server) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.UUID)
	}
	return out
}
