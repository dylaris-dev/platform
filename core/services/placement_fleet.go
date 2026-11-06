package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"

	"dylaris-core/models"
	"dylaris-core/store"

	"dylaris-pkg/storageplacement"

	"github.com/redis/go-redis/v9"
)

// FleetPlacementKey holds the placement policy a node uses when it has none of
// its own. It sits under dylaris:placement: because that prefix is already in
// every node's read ACL.
//
// A fleet-wide ORDER is deliberately more useful than a single "default path"
// would be: OrderPaths already drops entries a node does not have and appends
// the ones it was not told about, so one list of ["/storage"] expresses
// "/storage first everywhere, anything else after it" across a mixed fleet
// without naming a single node.
const FleetPlacementKey = "dylaris:placement:storage_default"

// LoadFleetPlacement reads the fleet default. Missing or unparseable means auto,
// the same fallback a node applies on its own.
func LoadFleetPlacement(ctx context.Context, rdb *redis.Client) NodePlacement {
	// Order is [] rather than nil so the JSON reads "order": [], never
	// "order": null - the panel feeds it straight into array operations, and a
	// fresh deploy with nothing saved crashed the whole Placement tab on
	// null.filter before this.
	out := NodePlacement{Mode: storageplacement.ModeAuto, Order: []string{}}
	if rdb == nil {
		return out
	}
	val, err := rdb.Get(ctx, FleetPlacementKey).Result()
	if err != nil {
		return out
	}
	return decodeFleetPlacement(val)
}

// decodeFleetPlacement parses a stored policy. An empty or malformed value is
// auto, never an error: an unreadable default must not stop a server from being
// placed, it just stops expressing a preference.
func decodeFleetPlacement(raw string) NodePlacement {
	out := NodePlacement{Mode: storageplacement.ModeAuto, Order: []string{}}
	if raw == "" {
		return out
	}
	var stored NodePlacement
	if json.Unmarshal([]byte(raw), &stored) != nil {
		return out
	}
	stored.Mode = storageplacement.NormalizeMode(stored.Mode)
	if stored.Order == nil {
		stored.Order = []string{}
	}
	return stored
}

// FleetPlacementSetting and NodePlacementSetting are the database rows behind
// the two Redis keys. Redis runs without persistence, so the keys alone lost an
// operator's placement to every Redis restart; the rows are what
// PublishStoragePlacements re-asserts from.
const FleetPlacementSetting = "placement.storage_default"

// NodePlacementSetting is keyed by node ID, not token, so a rolled key keeps it.
func NodePlacementSetting(nodeID int) string {
	return "node." + strconv.Itoa(nodeID) + ".storage_placement"
}

// SaveFleetPlacement writes the fleet default, database first. No TTL: it is
// configuration, and nodes re-read it on every settings poll.
func SaveFleetPlacement(ctx context.Context, st store.Store, rdb *redis.Client, p NodePlacement) error {
	return savePlacement(ctx, st, rdb, FleetPlacementSetting, FleetPlacementKey, p)
}

// SaveNodePlacement writes one node's policy, database first.
func SaveNodePlacement(ctx context.Context, st store.Store, rdb *redis.Client, node models.Node, p NodePlacement) error {
	return savePlacement(ctx, st, rdb, NodePlacementSetting(node.ID), NodePlacementKey(node.Token), p)
}

func savePlacement(ctx context.Context, st store.Store, rdb *redis.Client, setting, key string, p NodePlacement) error {
	p.Mode = storageplacement.NormalizeMode(p.Mode)
	if p.Order == nil {
		p.Order = []string{}
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if st != nil {
		if err := st.SetSetting(setting, string(payload)); err != nil {
			return fmt.Errorf("store placement %s: %w", setting, err)
		}
	}
	if rdb == nil {
		return nil
	}
	return rdb.Set(ctx, key, payload, 0).Err()
}

// PublishStoragePlacements re-asserts the fleet default and every node's policy
// from the database. A row that was never saved adopts what Redis holds, which
// is how a policy saved by a Core from before the rows existed gets one.
func PublishStoragePlacements(ctx context.Context, st store.Store, rdb *redis.Client) {
	if st == nil || rdb == nil {
		return
	}
	publishPlacement(ctx, st, rdb, FleetPlacementSetting, FleetPlacementKey)
	nodes, err := st.ListNodes()
	if err != nil {
		log.Printf("placement publisher: list nodes: %v", err)
		return
	}
	for _, n := range nodes {
		if n.Token == "" {
			continue
		}
		publishPlacement(ctx, st, rdb, NodePlacementSetting(n.ID), NodePlacementKey(n.Token))
	}
}

func publishPlacement(ctx context.Context, st store.Store, rdb *redis.Client, setting, key string) {
	v, err := st.GetSetting(setting)
	if err == nil && v != "" {
		if err := rdb.Set(ctx, key, v, 0).Err(); err != nil {
			log.Printf("placement publisher: set %s: %v", key, err)
		}
		return
	}
	// Only a row that does not exist is adopted: an unreadable one must not be
	// overwritten with whatever Redis holds.
	if !errors.Is(err, sql.ErrNoRows) {
		return
	}
	cur, rerr := rdb.Get(ctx, key).Result()
	if rerr != nil || cur == "" {
		return
	}
	if err := st.SetSetting(setting, cur); err != nil {
		log.Printf("placement publisher: adopt %s: %v", setting, err)
	}
}

// EffectiveNodePlacement is the policy a node actually applies: its own if it
// has one, otherwise the fleet default. Mirrors the node's own resolution so
// Core's capacity prediction stays truthful.
//
// "Has one" means a manual mode or a non-empty order. A node stored as plain
// auto with no order carries no information, so it falls through to the fleet
// default rather than pinning the node to auto forever.
func EffectiveNodePlacement(ctx context.Context, rdb *redis.Client, nodeToken string) NodePlacement {
	return resolvePlacement(LoadNodePlacement(ctx, rdb, nodeToken), LoadFleetPlacement(ctx, rdb))
}

// resolvePlacement is the precedence rule on its own, so it can be tested
// without Redis. It decides which disk a server lands on, and Core has to reach
// the same answer the node will.
func resolvePlacement(own, fleet NodePlacement) NodePlacement {
	if own.Mode == storageplacement.ModeManual || len(own.Order) > 0 {
		return own
	}
	return fleet
}

// FleetStoragePaths is every storage path any online node reports, deduplicated
// and sorted. It is what the fleet-default editor offers, since no single node's
// path list describes the fleet.
//
// It used to claim "first-seen order", which was never true: the heartbeats
// arrive as a map and Go randomises map iteration, so the editor's list
// reshuffled on every load.
func FleetStoragePaths(ctx context.Context, rdb *redis.Client) []string {
	return collectStoragePaths(LoadHeartbeats(ctx, rdb))
}

// collectStoragePaths gathers every distinct path the fleet reports. Sorted
// rather than first-seen: Go randomises map iteration, so a first-seen order
// would reshuffle the editor's list on every page load.
func collectStoragePaths(heartbeats map[string]*NodeHeartbeat) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, hb := range heartbeats {
		if hb == nil {
			continue
		}
		for _, sp := range hb.Storage {
			if sp.Path == "" || seen[sp.Path] {
				continue
			}
			seen[sp.Path] = true
			out = append(out, sp.Path)
		}
	}
	sort.Strings(out)
	return out
}
