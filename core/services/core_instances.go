package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/redis/go-redis/v9"
)

// This is the READ side of the heartbeats CoreHeartbeatService writes. Until
// now the only reader lived in the node agent (node/grpc_mesh.go scanCores),
// so nothing under core/ could answer "how many Cores are online?" - which is
// the question the host-path storage backend depends on, because a host path
// is only correct on a single Core.
//
// Note for anyone tempted to derive an instance id from CLUSTER_SECRET: that
// value is per-DEPLOYMENT (identical on every replica), so it is architecturally
// incapable of counting instances.

// coreLeaderKey shares the dylaris:core: prefix with the heartbeats but is the
// leader lease, not an instance registry - it holds the current leader's id as
// a bare string. Counting it would report two Cores on a single-Core install.
// It is excluded by name AND, as a second line of defence, by the requirement
// below that a value parse as a heartbeat with a non-empty id.
const coreLeaderKey = "dylaris:core:leader"

// coreScanKeyBudget bounds the SCAN loop. The keyspace being walked is written
// only by Cores with a 30s TTL, so anything past this is a corrupted or
// hostile keyspace rather than a real cluster; stopping is better than letting
// one HTTP request walk an unbounded keyspace.
const coreScanKeyBudget = 10000

// OnlineCoreIDs returns just the ids, for the callers that only need to know
// how many Cores are online. It is the same walk as OnlineCores, so the two can
// never disagree about who is up.
func OnlineCoreIDs(ctx context.Context, rdb *redis.Client) ([]string, error) {
	cores, err := OnlineCores(ctx, rdb)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(cores))
	for _, c := range cores {
		ids = append(ids, c.ID)
	}
	return ids, nil
}

// OnlineCores returns the heartbeat of every Core instance currently
// heartbeating, sorted by id and deduplicated. The version each carries is
// what lets a reader report the whole fleet rather than only the process it
// happens to be talking to.
//
// The key budget is checked once per SCAN batch rather than per key, and the
// COUNT passed to SCAN is a hint the server may exceed, so the real bound is
// coreScanKeyBudget plus at most one batch. Stating it as a hard "at most"
// would be wrong; the point of the budget is that the walk is bounded at all.
//
// SCAN rather than KEYS (which node/grpc_mesh.go uses): KEYS blocks the whole
// Redis server for the length of the walk, and this runs on an HTTP request
// path shared with every other subsystem on the same Redis.
//
// A count of 0 is possible and is NOT an error: the caller's own heartbeat may
// not have landed yet, or may have failed to write. Callers must treat 0 and 1
// alike - "not more than one" - and never read 0 as "no Cores exist", because
// the process asking is itself a Core.
//
// Two known ways the count is not the literal number of running processes, both
// of which a caller that gates on it has to live with:
//   - It can UNDERCOUNT. Instances are distinguished by DYLARIS_CORE_ID, which
//     defaults to the hostname. Two Cores configured with the same id write the
//     same key and are indistinguishable here.
//   - It can OVERCOUNT for up to 30s (the heartbeat TTL) after a Core stops
//     WITHOUT running its shutdown path - SIGKILL, OOM, host failure - because
//     the key then expires rather than being deleted. A Core that shuts down
//     cleanly deletes its own key (CoreHeartbeatService.Stop), so an orderly
//     rolling restart no longer shows the outgoing instance.
func OnlineCores(ctx context.Context, rdb *redis.Client) ([]CoreHeartbeat, error) {
	if rdb == nil {
		return nil, fmt.Errorf("core instances: no redis client")
	}

	seen := make(map[string]CoreHeartbeat)
	var cursor uint64
	examined := 0

	for {
		keys, next, err := rdb.Scan(ctx, cursor, "dylaris:core:*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("core instances: scan: %w", err)
		}

		for _, key := range keys {
			examined++
			if key == coreLeaderKey || key == CoreIndexKey {
				continue
			}

			val, err := rdb.Get(ctx, key).Result()
			if err != nil {
				if errors.Is(err, redis.Nil) {
					// A key that expired between the SCAN and this GET is the
					// normal race, not a failure: the heartbeat it belonged to
					// is stale by definition, so skipping it is the right
					// answer.
					continue
				}
				// Any other failure is Redis breaking, and it must NOT be read
				// as an absent key. Swallowing it returns a silently SHORT list
				// with a nil error, and the shared-storage save path reads a
				// short list as "not more than one Core online", which is its
				// signal that there is nothing to prove: it skips the whole
				// verification round and persists a backend no Core ever
				// demonstrated it could reach - reported to the admin as a pass.
				// Same reasoning and the same idiom as storagereach.Faults.
				return nil, fmt.Errorf("core instances: get %s: %w", key, err)
			}

			var hb CoreHeartbeat
			if err := json.Unmarshal([]byte(val), &hb); err != nil {
				continue
			}
			if hb.ID == "" {
				continue
			}
			// Keyed on the id inside the payload rather than the key name, so
			// a stray key holding a duplicate heartbeat cannot inflate the
			// count past the number of distinct instances.
			if _, dup := seen[hb.ID]; !dup {
				seen[hb.ID] = hb
			}
		}

		cursor = next
		if cursor == 0 || examined >= coreScanKeyBudget {
			break
		}
	}

	out := make([]CoreHeartbeat, 0, len(seen))
	for _, hb := range seen {
		out = append(out, hb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
