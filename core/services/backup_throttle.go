package services

import (
	"context"
	"database/sql"
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
	t, _ := loadNodeBackupThrottle(st.GetSetting, nodeID)
	return t
}

// settingReadFailed is a read error that is not "never saved".
func settingReadFailed(err error) bool {
	return err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, store.ErrSettingNull)
}

// loadNodeBackupThrottle errs only on a failed read: the publisher must not
// send that as "nothing set", the node would switch pacing and spool off.
func loadNodeBackupThrottle(get func(string) (string, error), nodeID int) (NodeBackupThrottle, error) {
	var t NodeBackupThrottle
	raw, err := get(NodeBackupThrottleSetting(nodeID))
	if settingReadFailed(err) {
		return NodeBackupThrottle{}, err
	}
	if raw != "" && json.Unmarshal([]byte(raw), &t) != nil {
		return NodeBackupThrottle{}, nil
	}
	return t, nil
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
	headroomPct, saveOffMaxMin, _ = backupThrottleGlobals(st.GetSetting)
	return headroomPct, saveOffMaxMin
}

// backupThrottleGlobals answers the defaults alongside a failed read's error.
func backupThrottleGlobals(get func(string) (string, error)) (headroomPct int64, saveOffMaxMin *int64, err error) {
	headroomPct = DefaultBackupThrottleHeadroomPct
	v, herr := get(SettingBackupThrottleHeadroomPct)
	if v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 && n <= 90 {
			headroomPct = n
		}
	}
	raw, cerr := get(SettingBackupThrottleSaveOffMax)
	saveOffMaxMin = ParseLimitSetting(raw, LimitPtr(DefaultBackupThrottleSaveOffMax))
	for _, e := range []error{herr, cerr} {
		if settingReadFailed(e) {
			return headroomPct, saveOffMaxMin, e
		}
	}
	return headroomPct, saveOffMaxMin, nil
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
	// ShareQuotaWithServer reaches a node that gets no backup_run, and
	// overrides the copy a backup_run carries. Omitted when unreadable.
	ShareQuotaWithServer *bool `json:"shareQuotaWithServer,omitempty"`
	// ShareQuotaUnreadable marks the omission, so the node keeps treating the
	// policy as the source instead of taking it for an older Core's.
	ShareQuotaUnreadable bool `json:"shareQuotaUnreadable,omitempty"`
}

// BackupPolicyJSON is what a node with these values is sent.
func BackupPolicyJSON(t NodeBackupThrottle, headroomPct int64, saveOffMaxMin *int64, share *bool) string {
	b, _ := json.Marshal(backupPolicy{
		UplinkMbit: t.UplinkMbit, ReadMBps: t.ReadMBps,
		HeadroomPct: headroomPct, SaveOffMaxMin: saveOffMaxMin,
		SpoolEnabled: t.SpoolEnabled, SpoolMaxGB: t.SpoolMaxGB,
		ShareQuotaWithServer: share, ShareQuotaUnreadable: share == nil,
	})
	return string(b)
}

// PublishBackupPolicy writes one node's policy now. Only a save calls it, after
// its commit: it bumps the save generation so an overlapping tick cannot
// write the pre-save policy over this one.
//
// A failed read publishes nothing: the node keeps the last policy until the
// next good tick. Sending the zero values instead switched pacing and spooling
// off for the length of a DB blip.
func PublishBackupPolicy(ctx context.Context, st store.Store, rdb *redis.Client, node models.Node) error {
	if rdb == nil || node.Token == "" {
		return nil
	}
	MarkNodeModeSave(ctx, rdb)
	get := store.FreshSettings(st)
	headroom, ceiling, err := backupThrottleGlobals(get)
	if err != nil {
		return fmt.Errorf("read backup throttle globals: %w", err)
	}
	t, err := loadNodeBackupThrottle(get, node.ID)
	if err != nil {
		return fmt.Errorf("read backup throttle (keeping the published policy): %w", err)
	}
	return rdb.Set(ctx, NodeBackupPolicyKey(node.Token), BackupPolicyJSON(t, headroom, ceiling, backupShareQuotaModeFrom(get)), 0).Err()
}

// PublishBackupPolicies writes every node's policy now, after a save of the
// global backup settings; the generation bump keeps an overlapping tick from
// undoing it.
func PublishBackupPolicies(ctx context.Context, st store.Store, rdb *redis.Client) {
	MarkNodeModeSave(ctx, rdb)
	for key, v := range backupPolicyWrites(st) {
		if err := rdb.Set(ctx, key, v, 0).Err(); err != nil {
			log.Printf("backup policy publisher: set %s: %v", key, err)
		}
	}
}

// republishBackupPolicies is the tick's PublishBackupPolicies, under WATCH on
// the save generation: its read can predate a save another replica has
// already mirrored, and its writes would put the old share mode back (which
// re-tags every backup folder on the node, then again on the next tick).
func republishBackupPolicies(ctx context.Context, st store.Store, rdb *redis.Client) {
	publishUnderSaveGen(ctx, rdb, "backup policy publisher", func(tx *redis.Tx) error {
		writes := backupPolicyWrites(st)
		if len(writes) == 0 {
			return nil
		}
		_, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			for key, v := range writes {
				pipe.Set(ctx, key, v, 0)
			}
			return nil
		})
		return err
	})
}

// backupPolicyWrites is every node's policy keyed by its Redis key. Every node
// gets one, set or not, so the global headroom and ceiling reach nodes paced
// by env alone.
//
// The settings come from one fresh read past the one-second snapshot: a
// snapshot from just before another replica's save would push the old values
// back to every node. A failed read leaves out what it covers.
func backupPolicyWrites(st store.Store) map[string]string {
	nodes, err := st.ListNodes()
	if err != nil {
		log.Printf("backup policy publisher: list nodes: %v", err)
		return nil
	}
	get := store.FreshSettings(st)
	headroom, ceiling, err := backupThrottleGlobals(get)
	if err != nil {
		log.Printf("backup policy publisher: read globals: %v (keeping the published policies)", err)
		return nil
	}
	share := backupShareQuotaModeFrom(get)
	out := map[string]string{}
	for _, n := range nodes {
		if n.Token == "" {
			continue
		}
		t, err := loadNodeBackupThrottle(get, n.ID)
		if err != nil {
			log.Printf("backup policy publisher: node %d: read backup throttle (keeping the published policy): %v", n.ID, err)
			continue
		}
		out[NodeBackupPolicyKey(n.Token)] = BackupPolicyJSON(t, headroom, ceiling, share)
	}
	return out
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
