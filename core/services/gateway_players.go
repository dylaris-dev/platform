package services

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// edgePlayersMaxAge is how old an edge's sample may be and still count: three
// of its 3 s publish intervals. A dead edge's last sample stays in its stream,
// and the players it held are by now on the sibling edge, so an older sample
// would count them twice.
const edgePlayersMaxAge = 9 * time.Second

// gatewayPlayersCacheTTL lets every panel that polls share one read of the
// edge streams.
const gatewayPlayersCacheTTL = 3 * time.Second

// RoutePlayers is the play sessions on one route-only address, by edition.
type RoutePlayers struct{ Java, Bedrock int }

type edgePlayersSample struct {
	at      time.Time
	players map[string]int
	routes  map[string]RoutePlayers
}

// sumFreshPlayers adds up the players per server across edges, from samples
// no older than edgePlayersMaxAge. During a splice handover the old and the
// new edge can both report a player for under one interval; that is the error
// this accepts.
func sumFreshPlayers(samples []edgePlayersSample, now time.Time) map[string]int {
	out := map[string]int{}
	for _, s := range samples {
		if now.Sub(s.at) > edgePlayersMaxAge {
			continue
		}
		for uuid, n := range s.players {
			if n > 0 {
				out[uuid] += n
			}
		}
	}
	return out
}

// sumFreshRoutePlayers is sumFreshPlayers for the per-address counts, under
// the same freshness rule.
func sumFreshRoutePlayers(samples []edgePlayersSample, now time.Time) map[string]RoutePlayers {
	out := map[string]RoutePlayers{}
	for _, s := range samples {
		if now.Sub(s.at) > edgePlayersMaxAge {
			continue
		}
		for domain, p := range s.routes {
			j, b := max(p.Java, 0), max(p.Bedrock, 0)
			if j+b == 0 {
				continue
			}
			sum := out[domain]
			sum.Java += j
			sum.Bedrock += b
			out[domain] = sum
		}
	}
	return out
}

// maxRouteEntriesPerSample bounds what Core keeps from one edge's sample, so a
// broken or hostile edge cannot make the provider hold an unbounded map.
const maxRouteEntriesPerSample = 10000

// parseRoutePlayers reads players_by_route on its own, so a malformed value
// loses only the route counts of that sample and never the server counts an
// older Core would have kept. Negative counts are ignored.
func parseRoutePlayers(raw json.RawMessage) map[string]RoutePlayers {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]struct {
		J int `json:"j"`
		B int `json:"b"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := map[string]RoutePlayers{}
	for d, p := range m {
		if len(out) >= maxRouteEntriesPerSample {
			break
		}
		j, b := max(p.J, 0), max(p.B, 0)
		if j+b > 0 {
			out[d] = RoutePlayers{Java: j, Bedrock: b}
		}
	}
	return out
}

var gatewayPlayersCache struct {
	sync.Mutex
	at     time.Time
	from   *redis.Client // a cached answer is only good for the client it was read through
	val    map[string]int
	routes map[string]RoutePlayers
}

// GatewayPlayersByServer is how many players are connected through the
// gateway to each server, by server UUID, as the edges count them. Empty when
// no edge publishes the count (an edge older than the field, or no gateway).
func GatewayPlayersByServer(ctx context.Context, rdb *redis.Client) map[string]int {
	servers, _ := gatewayPlayers(ctx, rdb)
	return servers
}

// GatewayPlayersByRoute is how many players are on each route-only address, by
// the route's stored domain, as the edges count them. Empty when no edge
// publishes the count. The map is shared with other callers: read it only.
func GatewayPlayersByRoute(ctx context.Context, rdb *redis.Client) map[string]RoutePlayers {
	_, routes := gatewayPlayers(ctx, rdb)
	return routes
}

// gatewayPlayers is the one read of the edge streams behind both maps.
func gatewayPlayers(ctx context.Context, rdb *redis.Client) (map[string]int, map[string]RoutePlayers) {
	gatewayPlayersCache.Lock()
	defer gatewayPlayersCache.Unlock()
	if gatewayPlayersCache.val != nil && gatewayPlayersCache.from == rdb && time.Since(gatewayPlayersCache.at) < gatewayPlayersCacheTTL {
		return gatewayPlayersCache.val, gatewayPlayersCache.routes
	}
	// Detached from the caller: this read is shared by every polling panel, and
	// one panel navigating away must not cache an empty answer for all of them.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var samples []edgePlayersSample
	for _, e := range GetEdgeIDsFromRedis(ctx, rdb) {
		msgs, err := rdb.XRevRangeN(ctx, "dylaris:edge:"+e+":stats", "+", "-", 1).Result()
		if err != nil || len(msgs) == 0 {
			continue
		}
		at, ok := streamIDTime(msgs[0].ID)
		raw, _ := msgs[0].Values["data"].(string)
		if !ok || raw == "" {
			continue
		}
		var s struct {
			Players map[string]int  `json:"players_by_server"`
			Routes  json.RawMessage `json:"players_by_route"`
		}
		if json.Unmarshal([]byte(raw), &s) != nil {
			continue
		}
		samples = append(samples, edgePlayersSample{at: at, players: s.Players, routes: parseRoutePlayers(s.Routes)})
	}
	now := time.Now()
	gatewayPlayersCache.val = sumFreshPlayers(samples, now)
	gatewayPlayersCache.routes = sumFreshRoutePlayers(samples, now)
	gatewayPlayersCache.at, gatewayPlayersCache.from = now, rdb
	return gatewayPlayersCache.val, gatewayPlayersCache.routes
}

// streamIDTime reads the millisecond timestamp Redis puts in a stream entry
// id ("1729700000000-0"). It is the time the entry was ADDED, set by Redis,
// so it does not depend on the edge's clock (only Core's against Redis's,
// which NTP keeps well inside edgePlayersMaxAge).
func streamIDTime(id string) (time.Time, bool) {
	ms, err := strconv.ParseInt(strings.SplitN(id, "-", 2)[0], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

// GetEdgeIDsFromRedis lists the ids of the edges in the registry.
func GetEdgeIDsFromRedis(ctx context.Context, rdb *redis.Client) []string {
	var ids []string
	for _, e := range listEdgeRegistry(ctx, rdb) {
		if e.EdgeID != "" {
			ids = append(ids, e.EdgeID)
		}
	}
	return ids
}
