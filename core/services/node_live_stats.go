package services

import (
	"context"
	"encoding/json"
	"fmt"

	"dylaris-core/models"

	"github.com/redis/go-redis/v9"
)

// EnrichNodesWithLiveStats fills the fields ListNodes cannot: the live
// heartbeat figures and the server count. It returns the set of node tokens a
// heartbeat was actually found for.
//
// Shared rather than repeated, because the two callers had drifted. The panel's
// infrastructure handler did this; the metrics collector took the same
// ListNodes result and read the fields straight off it. Those fields are
// documented on models.Node as "live stats from heartbeat (not persisted)", so
// on an unenriched struct they are the zero value - and the collector's guard
// is `CPUUsage >= 0`, which a zero passes.
//
// Measured in production on 2026-09-03: node.cpu_pct and node.servers held 1218
// rows each, every one of them 0, while the machine was running two servers;
// node.ram_pct and node.ram_used_bytes had never been written at all, because
// their guard is `RAMTotal > 0` and an unenriched RAMTotal is 0. The catalogue
// offered all four.
//
// The returned set is what lets a caller tell "measured 0%" from "no
// measurement", which the struct itself cannot express: an absent heartbeat and
// a genuinely idle machine both leave 0 behind.
func EnrichNodesWithLiveStats(ctx context.Context, st NodeServerCounter, rdb *redis.Client, nodes []models.Node) map[string]bool {
	seen := make(map[string]bool, len(nodes))
	for i := range nodes {
		if st != nil {
			if count, err := st.CountServersByNode(nodes[i].ID); err == nil {
				nodes[i].ServerCount = count
			}
		}
		hb, ok := LoadNodeHeartbeat(ctx, rdb, nodes[i].Token)
		if !ok {
			continue
		}
		seen[nodes[i].Token] = true
		nodes[i].CPUUsage = hb.CPUUsage
		nodes[i].RAMFree = hb.RAMFree
		nodes[i].RAMTotal = hb.RAMTotal
		nodes[i].LinkCount = hb.LinkCount
		nodes[i].PortRange = hb.PortRange
		nodes[i].PortRangeNotice = hb.PortRangeNotice
		nodes[i].SharedStorage = hb.SharedStorage
		nodes[i].Isolation = hb.Isolation
		nodes[i].IsolationNotice = hb.IsolationNotice
	}
	return seen
}

// NodeServerCounter is the one store method this needs, kept narrow so the
// collector's tests do not have to build a whole store.
type NodeServerCounter interface {
	CountServersByNode(nodeID int) (int, error)
}

// LoadNodeHeartbeat reads one node's heartbeat. The second return is false when
// there is none - the node has not reported within its key's lifetime, or Redis
// is unreachable - which is a different thing from a node reporting zeros.
func LoadNodeHeartbeat(ctx context.Context, rdb *redis.Client, token string) (NodeHeartbeat, bool) {
	var hb NodeHeartbeat
	if rdb == nil || token == "" {
		return hb, false
	}
	// The node writes this every 5s, so values may be up to that stale.
	val, err := rdb.Get(ctx, fmt.Sprintf("dylaris:discovery:%s", token)).Result()
	if err != nil || val == "" {
		return hb, false
	}
	if json.Unmarshal([]byte(val), &hb) != nil {
		return hb, false
	}
	return hb, true
}
