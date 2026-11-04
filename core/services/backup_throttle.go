package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Backups yield to players. Each node paces its backup upload to what its
// uplink can spare while anyone is online on it, and can spool the archive to
// local disk so world saving resumes before the slow upload. The node does the
// pacing (node/backup_throttle.go); Core only owns the settings and publishes
// them to dylaris:node:<token>:backup_policy, a key the node's own ACL grant
// already covers.
//
// A missing key is the node's "no policy" - what an older Core leaves - so a
// node is only ever paced when someone set an uplink, here or in its env.

const (
	// SettingBackupThrottleHeadroomPct is the share of a node's uplink kept for
	// players on top of their measured traffic.
	SettingBackupThrottleHeadroomPct = "backup.throttle_headroom_pct"
	// SettingBackupThrottleSaveOffMax ends the pacing after this many minutes
	// of world saving being off. A limit setting: "unlimited" is no ceiling.
	SettingBackupThrottleSaveOffMax = "backup.throttle_saveoff_max_min"

	DefaultBackupThrottleHeadroomPct = 30
	DefaultBackupThrottleSaveOffMax  = 20

	maxUplinkMbit = 1_000_000 // 1 Tbit/s
	maxReadMBps   = 100_000
	maxSpoolGB    = 1_000_000
)

// NodeBackupThrottleSetting is the settings key for one node's values. In the
// settings table rather than a column: nothing queries them but this file.
func NodeBackupThrottleSetting(nodeID int) string {
	return "node." + strconv.Itoa(nodeID) + ".backup_throttle"
}

// NodeBackupPolicyKey is where the node reads its policy.
func NodeBackupPolicyKey(nodeToken string) string {
	return "dylaris:node:" + nodeToken + ":backup_policy"
}

// NodeBackupEnvKey is what the node reports about itself: its env uplink and
// the spool directory's free space.
func NodeBackupEnvKey(nodeToken string) string {
	return "dylaris:node:" + nodeToken + ":backup_env"
}

// NodeBackupThrottle is one node's values. Nil is "not set": no uplink means
// the node's NODE_UPLINK_MBIT, else no pacing; no read cap means none.
//
// None of them can be 0. A zero rate is not a cap, it is a backup that never
// finishes while world saving is off - so the smallest value is 1.
type NodeBackupThrottle struct {
	UplinkMbit   *int64 `json:"uplinkMbit"`
	ReadMBps     *int64 `json:"readMBps"`
	SpoolEnabled bool   `json:"spoolEnabled"`
	// SpoolMaxGB is the largest server, uncompressed, that is spooled.
	SpoolMaxGB int64 `json:"spoolMaxGB"`
}

func (t NodeBackupThrottle) Validate() error {
	inRange := func(v *int64, hi int64) bool { return v == nil || (*v >= 1 && *v <= hi) }
	switch {
	case !inRange(t.UplinkMbit, maxUplinkMbit):
		return fmt.Errorf("uplink must be 1 to %d Mbit/s, or unset", maxUplinkMbit)
	case !inRange(t.ReadMBps, maxReadMBps):
		return fmt.Errorf("read cap must be 1 to %d MB/s, or unset", maxReadMBps)
	case t.SpoolEnabled && (t.SpoolMaxGB < 1 || t.SpoolMaxGB > maxSpoolGB):
		return errors.New("spooling needs a largest backup size of at least 1 GB")
	case t.SpoolMaxGB < 0:
		return errors.New("the largest spooled backup cannot be negative")
	}
	return nil
}

// LoadNodeBackupThrottle reads a node's values; unset or unreadable is all nil.
func LoadNodeBackupThrottle(st store.Store, nodeID int) NodeBackupThrottle {
	var t NodeBackupThrottle
	raw, _ := st.GetSetting(NodeBackupThrottleSetting(nodeID))
	if raw != "" && json.Unmarshal([]byte(raw), &t) != nil {
		return NodeBackupThrottle{}
	}
	return t
}

func SaveNodeBackupThrottle(st store.Store, nodeID int, t NodeBackupThrottle) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return st.SetSetting(NodeBackupThrottleSetting(nodeID), string(b))
}

// BackupThrottleGlobals reads the platform-wide pair.
func BackupThrottleGlobals(st store.Store) (headroomPct int64, saveOffMaxMin *int64) {
	headroomPct = DefaultBackupThrottleHeadroomPct
	if v, _ := st.GetSetting(SettingBackupThrottleHeadroomPct); v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 && n <= 90 {
			headroomPct = n
		}
	}
	raw, _ := st.GetSetting(SettingBackupThrottleSaveOffMax)
	return headroomPct, ParseLimitSetting(raw, LimitPtr(DefaultBackupThrottleSaveOffMax))
}

// backupPolicy is the wire shape the node parses (node/backup_throttle.go).
// Fields may be added, never renamed: nodes and Core update separately.
type backupPolicy struct {
	UplinkMbit    *int64 `json:"uplinkMbit"`
	ReadMBps      *int64 `json:"readMBps"`
	HeadroomPct   int64  `json:"headroomPct"`
	SaveOffMaxMin *int64 `json:"saveOffMaxMin"`
	SpoolEnabled  bool   `json:"spoolEnabled"`
	SpoolMaxGB    int64  `json:"spoolMaxGB"`
}

// BackupPolicyJSON is what a node with these values is sent.
func BackupPolicyJSON(t NodeBackupThrottle, headroomPct int64, saveOffMaxMin *int64) string {
	b, _ := json.Marshal(backupPolicy{
		UplinkMbit: t.UplinkMbit, ReadMBps: t.ReadMBps,
		HeadroomPct: headroomPct, SaveOffMaxMin: saveOffMaxMin,
		SpoolEnabled: t.SpoolEnabled, SpoolMaxGB: t.SpoolMaxGB,
	})
	return string(b)
}

// PublishBackupPolicy writes one node's policy now (after a save).
func PublishBackupPolicy(ctx context.Context, st store.Store, rdb *redis.Client, node models.Node) error {
	if rdb == nil || node.Token == "" {
		return nil
	}
	headroom, ceiling := BackupThrottleGlobals(st)
	return publishBackupPolicy(ctx, st, rdb, node, headroom, ceiling)
}

func publishBackupPolicy(ctx context.Context, st store.Store, rdb *redis.Client, node models.Node, headroom int64, ceiling *int64) error {
	return rdb.Set(ctx, NodeBackupPolicyKey(node.Token), BackupPolicyJSON(LoadNodeBackupThrottle(st, node.ID), headroom, ceiling), 0).Err()
}

// PublishBackupPolicies writes every node's policy. Every node gets one, set
// or not, so the global headroom and ceiling reach nodes paced by env alone.
func PublishBackupPolicies(ctx context.Context, st store.Store, rdb *redis.Client) {
	nodes, err := st.ListNodes()
	if err != nil {
		log.Printf("backup policy publisher: list nodes: %v", err)
		return
	}
	headroom, ceiling := BackupThrottleGlobals(st)
	for _, n := range nodes {
		if n.Token == "" {
			continue
		}
		if err := publishBackupPolicy(ctx, st, rdb, n, headroom, ceiling); err != nil {
			log.Printf("backup policy publisher: node %d: %v", n.ID, err)
		}
	}
}

// NodeBackupEnv is the node's report, nil fields when it never reported (an
// older node, or one that is offline).
type NodeBackupEnv struct {
	UplinkMbit     *int64  `json:"uplinkMbit"`
	SpoolDir       *string `json:"spoolDir"`
	SpoolFreeBytes *uint64 `json:"spoolFreeBytes"`
}

func LoadNodeBackupEnv(ctx context.Context, rdb *redis.Client, nodeToken string) NodeBackupEnv {
	var env NodeBackupEnv
	if rdb == nil || nodeToken == "" {
		return env
	}
	raw, err := rdb.Get(ctx, NodeBackupEnvKey(nodeToken)).Result()
	if err == nil {
		_ = json.Unmarshal([]byte(raw), &env)
	}
	return env
}
