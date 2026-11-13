package services

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
)

// LinkOnline answers, for a set of link tunnel tokens, which of them a Link is
// currently keeping alive.
//
// The signal is the key a running Link refreshes every few seconds with a 15s
// TTL (`online_link:<token>`, gateway/link/providers.go). It is read here rather
// than derived from anything Core stores, because everything Core stores says
// what SHOULD be running: only this key says something is.
//
// nil means NOT KNOWN, and the difference from "offline" is the whole point of
// the return type. A missing Redis or a failed read is not evidence that a
// customer's machine is down, and a screen that says "not connected" because
// Core could not ask sends them to debug a machine that is fine. Callers must
// render nil as "no answer", never as offline.
//
// One round trip for the whole set: this backs list endpoints, and a call per
// row is how a list of twenty machines becomes twenty round trips.
func LinkOnline(ctx context.Context, rdb *redis.Client, tokens []string) map[string]bool {
	if rdb == nil || len(tokens) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(tokens))
	pipe := rdb.Pipeline()
	cmds := make(map[string]*redis.IntCmd, len(tokens))
	for _, t := range tokens {
		if t == "" || cmds[t] != nil {
			continue
		}
		cmds[t] = pipe.Exists(ctx, "online_link:"+t)
	}
	if len(cmds) == 0 {
		return nil
	}
	// A failed Exec gives up on the whole set rather than reading what came back.
	// Measured, not assumed: against a Redis that is gone, the individual
	// commands come back (0, nil) - their zero value, not an error - so trusting
	// them per command reported every Link as not connected, which is the one
	// answer this function exists to avoid.
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil
	}
	for t, cmd := range cmds {
		n, err := cmd.Result()
		if err != nil {
			continue
		}
		seen[t] = n > 0
	}
	if len(seen) == 0 {
		return nil
	}
	return seen
}

// EffectiveLinkToken is the token of the Link that serves a node: the one the
// Hub named for it, else the derived one. Exported for the API layer, which has
// to ask about the same Link the routes point at - asking about the derived
// token of a node served by a self-enrolled Link would report every such machine
// as not connected.
func EffectiveLinkToken(n *models.Node, clusterSecret string) string {
	return effectiveLinkToken(n, clusterSecret)
}

// LinkPresence is LinkOnline plus each online Link's announced capabilities,
// from ONE pipelined GET per token instead of an EXISTS round trip and then a
// GET per online Link. online has LinkOnline's contract (nil = not known);
// caps holds an entry only for an online Link that announced some (an old Link
// writes "1", which announces nothing).
func LinkPresence(ctx context.Context, rdb *redis.Client, tokens []string) (online map[string]bool, caps map[string][]string) {
	if rdb == nil || len(tokens) == 0 {
		return nil, nil
	}
	pipe := rdb.Pipeline()
	cmds := make(map[string]*redis.StringCmd, len(tokens))
	for _, t := range tokens {
		if t == "" || cmds[t] != nil {
			continue
		}
		cmds[t] = pipe.Get(ctx, "online_link:"+t)
	}
	if len(cmds) == 0 {
		return nil, nil
	}
	// redis.Nil is a missing key (an offline Link), not a failed read; any
	// other Exec error gives up on the whole set, as LinkOnline does.
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, nil
	}
	online = make(map[string]bool, len(cmds))
	caps = map[string][]string{}
	for t, cmd := range cmds {
		v, err := cmd.Result()
		switch {
		case errors.Is(err, redis.Nil):
			online[t] = false
		case err != nil:
			continue
		default:
			online[t] = true
			var p struct {
				Caps []string `json:"caps"`
			}
			if json.Unmarshal([]byte(v), &p) == nil && p.Caps != nil {
				caps[t] = p.Caps
			}
		}
	}
	if len(online) == 0 {
		return nil, nil
	}
	return online, caps
}
