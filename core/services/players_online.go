package services

import (
	"context"
	"dylaris-core/models"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// playersFreshFor is how old a server's newest stats sample may be and still
// count. The node writes one every 2 s while the container runs and stops
// writing when it goes away, while the stream itself stays behind - so an old
// last entry is a server that stopped, not one that still has its players.
const playersFreshFor = 60 * time.Second

// playerSample is one server's player count at one moment.
type playerSample struct {
	server  string
	at      time.Time
	players int
}

// sumLatestPlayers is the platform's players online: the NEWEST fresh sample
// of each server, added across servers.
//
// Per server first, because two samples of one server are the same players
// counted twice. That is what the old edge-stream figure got wrong in a
// different way: it counted connections, not people.
func sumLatestPlayers(samples []playerSample, now time.Time) int {
	latest := make(map[string]playerSample, len(samples))
	for _, s := range samples {
		// Both ways: a sample from the future is a clock or an ID somebody
		// chose, and would otherwise stay "fresh" for as long as it is ahead.
		if age := now.Sub(s.at); age > playersFreshFor || age < -playersFreshFor {
			continue
		}
		if cur, ok := latest[s.server]; !ok || s.at.After(cur.at) {
			latest[s.server] = s
		}
	}
	total := 0
	for _, s := range latest {
		if s.players > 0 {
			total += s.players
		}
	}
	return total
}

// PlayersOnline reads the newest stats sample of every server and returns how
// many players are on them right now, as their own server-list pings report it.
//
// The source is the per-server stats buffer the node already writes and the
// server page already reads, so this adds no instrumentation anywhere. One
// pipelined round trip for the whole fleet.
func PlayersOnline(ctx context.Context, rdb *redis.Client, servers []models.Server, now time.Time) (int, error) {
	// A proxy's ping reports every player on its network, and each backend
	// reports its own: counting both doubles the network. Players are where
	// they play, so proxies are left out.
	counted := make([]models.Server, 0, len(servers))
	for _, s := range servers {
		if s.ServerType != "proxy" {
			counted = append(counted, s)
		}
	}
	servers = counted
	if rdb == nil || len(servers) == 0 {
		return 0, nil
	}
	pipe := rdb.Pipeline()
	cmds := make([]*redis.XMessageSliceCmd, len(servers))
	for i := range servers {
		cmds[i] = pipe.XRevRangeN(ctx, fmt.Sprintf("dylaris:server:%s:stats:buffer", servers[i].UUID), "+", "-", 1)
	}
	// A missing stream answers an empty list, not an error, so an error here is
	// Redis itself - and a 0 for it would read as an empty platform.
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("read server stats: %w", err)
	}
	samples := make([]playerSample, 0, len(servers))
	for i, cmd := range cmds {
		msgs, err := cmd.Result()
		if err != nil || len(msgs) == 0 {
			continue
		}
		data, ok := msgs[0].Values["data"].(string)
		if !ok {
			continue
		}
		// Aged by the entry ID, which Redis stamps from its own clock, rather
		// than by the payload's `ts`: that one is the NODE's clock, and a
		// customer's machine with a wrong one would count forever or never.
		ms, err := strconv.ParseInt(strings.SplitN(msgs[0].ID, "-", 2)[0], 10, 64)
		if err != nil {
			continue
		}
		var p struct {
			Players int `json:"players"`
		}
		if json.Unmarshal([]byte(data), &p) != nil {
			continue
		}
		samples = append(samples, playerSample{server: servers[i].UUID, at: time.UnixMilli(ms), players: p.Players})
	}
	return sumLatestPlayers(samples, now), nil
}
