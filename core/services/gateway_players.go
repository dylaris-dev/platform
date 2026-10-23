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

type edgePlayersSample struct {
	at      time.Time
	players map[string]int
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

var gatewayPlayersCache struct {
	sync.Mutex
	at  time.Time
	val map[string]int
}

// GatewayPlayersByServer is how many players are connected through the
// gateway to each server, by server UUID, as the edges count them. Empty when
// no edge publishes the count (an edge older than the field, or no gateway).
func GatewayPlayersByServer(ctx context.Context, rdb *redis.Client) map[string]int {
	gatewayPlayersCache.Lock()
	defer gatewayPlayersCache.Unlock()
	if gatewayPlayersCache.val != nil && time.Since(gatewayPlayersCache.at) < gatewayPlayersCacheTTL {
		return gatewayPlayersCache.val
	}
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
			Players map[string]int `json:"players_by_server"`
		}
		if json.Unmarshal([]byte(raw), &s) != nil {
			continue
		}
		samples = append(samples, edgePlayersSample{at: at, players: s.Players})
	}
	gatewayPlayersCache.val = sumFreshPlayers(samples, time.Now())
	gatewayPlayersCache.at = time.Now()
	return gatewayPlayersCache.val
}

// streamIDTime reads the millisecond timestamp Redis puts in a stream entry
// id ("1729700000000-0"). It is the time the entry was ADDED, set by Redis,
// so it does not depend on the edge's clock.
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
