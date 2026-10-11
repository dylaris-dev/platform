package services

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"dylaris-core/store"

	"dylaris-pkg/storageplacement"

	"github.com/redis/go-redis/v9"
)

const (
	// DefaultDiskHeadroomGB is the free space a storage path must still have
	// after a placement decision. It replaces a bare literal 5 that was
	// duplicated in the scheduler and the rebalancer; 5 GB is far too little to
	// absorb a single server's growth on a modern disk.
	DefaultDiskHeadroomGB = 50
	// MinDiskHeadroomGB is the floor an operator may configure. Below this a
	// path can be filled to the point where the host itself misbehaves, which
	// takes down every server on it, not just the one that overran.
	MinDiskHeadroomGB = 10
	// DiskHeadroomSetting is the settings key holding the configured value. It
	// is the pre-existing "disk buffer" field in Settings -> Placement, which
	// was written and displayed but never actually read by any placement
	// decision - the checks used a hardcoded 5 GB instead. Wiring it here makes
	// the control the admin already sees do what it says.
	DiskHeadroomSetting = "placement.disk_buffer_gb"
)

// DiskHeadroomGB returns the configured headroom, clamped to MinDiskHeadroomGB.
// An unset, unparseable or too-small value falls back to the default rather than
// to something permissive: the whole point of the setting is a floor.
func DiskHeadroomGB(s store.Store) int64 {
	if s == nil {
		return DefaultDiskHeadroomGB
	}
	raw, _ := s.GetSetting(DiskHeadroomSetting)
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < MinDiskHeadroomGB {
		return DefaultDiskHeadroomGB
	}
	return n
}

// NodePlacement is a node's storage placement policy, as stored in Redis by the
// panel and read by the node itself. Core reads it to PREDICT which path the
// node will choose.
type NodePlacement struct {
	Mode  string   `json:"mode"`
	Order []string `json:"order"`
}

// NodePlacementKey is where a node's policy lives. Per node, because the paths
// differ per node and a fleet-wide key could not name them.
func NodePlacementKey(nodeToken string) string {
	return "dylaris:node:" + nodeToken + ":storage_placement"
}

// LoadNodePlacement reads a node's policy. Missing, unreadable or unparseable
// all mean auto - the same fallback the node applies, so Core never predicts a
// behaviour the node would not exhibit.
func LoadNodePlacement(ctx context.Context, rdb *redis.Client, nodeToken string) NodePlacement {
	out := NodePlacement{Mode: storageplacement.ModeAuto}
	if rdb == nil || nodeToken == "" {
		return out
	}
	val, err := rdb.Get(ctx, NodePlacementKey(nodeToken)).Result()
	if err != nil || val == "" {
		return out
	}
	var stored NodePlacement
	if json.Unmarshal([]byte(val), &stored) != nil {
		return out
	}
	stored.Mode = storageplacement.NormalizeMode(stored.Mode)
	return stored
}

// PickPlacementPath returns the storage path the NODE would choose for a new
// server, plus that path's free bytes.
//
// Predicting the node's choice is the whole point. Measuring the LARGEST path
// instead - which is what both callers used to do - is wrong as soon as a
// manual priority order exists: the node may deliberately place onto a smaller
// disk, so Core would admit a server against capacity that will not be used for
// it. The fallbacks mirror the node exactly, including dropping through to
// free-space selection when no path in the order is usable.
func PickPlacementPath(storage []HeartbeatStoragePath, p NodePlacement) (string, int64) {
	free := make(map[string]int64, len(storage))
	paths := make([]string, 0, len(storage))
	for _, sp := range storage {
		if sp.Path == "" {
			continue
		}
		free[sp.Path] = sp.FreeBytes
		paths = append(paths, sp.Path)
	}
	if len(paths) == 0 {
		return "", 0
	}

	if storageplacement.NormalizeMode(p.Mode) == storageplacement.ModeManual {
		for _, path := range storageplacement.OrderPaths(paths, p.Order) {
			if free[path] > 0 {
				return path, free[path]
			}
		}
	}

	best, bestFree := "", int64(0)
	for _, path := range paths {
		if free[path] > bestFree {
			best, bestFree = path, free[path]
		}
	}
	return best, bestFree
}

// CommittedBytesByPath sums, per storage path, the disk limits already PROMISED
// to the servers that path carries.
//
// This is the number free space cannot show. Twenty servers with a 50 GB limit
// each have committed a terabyte the moment they are created, even while the
// directories are nearly empty - and every one of them is entitled to grow into
// it. Placing against free space alone therefore oversubscribes silently and
// only fails much later, when users actually fill their servers.
//
// limitMB is uuid -> disk limit in MEGABYTES (the unit the servers table and the
// node's quota calls use). A server with limit 0 is unlimited and contributes
// nothing: no promise was made, so none can be reserved.
func CommittedBytesByPath(storage []HeartbeatStoragePath, limitMB map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(storage))
	for _, sp := range storage {
		var sum int64
		for _, uuid := range sp.ServerUUIDs {
			if mb := limitMB[uuid]; mb > 0 {
				sum += mb * 1024 * 1024
			}
		}
		out[sp.Path] = sum
	}
	return out
}

// UnwrittenBytesByPath sums, per storage path, what the servers there may still
// write: max(0, limit - measured usage) per server.
//
// It is per SERVER on purpose. The path's UsedBytes is the whole filesystem -
// the OS, Docker images, anything else on the disk - so subtracting it from the
// promises (the old formula) clamped to zero on any shared disk and reported the
// full free space as promisable.
//
// usedBytes is uuid -> the node's measured usage. A server with no measurement
// counts as having written nothing, so its whole limit stays reserved: an
// unknown must never make more room appear.
func UnwrittenBytesByPath(storage []HeartbeatStoragePath, limitMB, usedBytes map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(storage))
	for _, sp := range storage {
		var sum int64
		for _, uuid := range sp.ServerUUIDs {
			if mb := limitMB[uuid]; mb > 0 {
				if left := mb*1024*1024 - usedBytes[uuid]; left > 0 {
					sum += left
				}
			}
		}
		out[sp.Path] = sum
	}
	return out
}

// LoadServerDiskUsage reads the node's per-server disk gauge
// (dylaris:server:<uuid>:stats:disk, JSON {"total": bytes}) for every server on
// the given paths. Missing or unreadable entries are simply absent, which
// UnwrittenBytesByPath treats as "nothing written yet".
func LoadServerDiskUsage(ctx context.Context, rdb *redis.Client, storage []HeartbeatStoragePath) map[string]int64 {
	out := map[string]int64{}
	if rdb == nil {
		return out
	}
	var uuids, keys []string
	for _, sp := range storage {
		for _, uuid := range sp.ServerUUIDs {
			uuids = append(uuids, uuid)
			keys = append(keys, "dylaris:server:"+uuid+":stats:disk")
		}
	}
	if len(keys) == 0 {
		return out
	}
	vals, err := rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return out
	}
	for i, v := range vals {
		raw, ok := v.(string)
		if !ok {
			continue
		}
		var gauge struct {
			Total int64 `json:"total"`
		}
		if json.Unmarshal([]byte(raw), &gauge) == nil && gauge.Total > 0 {
			out[uuids[i]] = gauge.Total
		}
	}
	return out
}

// DiskCapacityRequest is one placement decision's inputs. Grouped into a struct
// because the parameters are easy to swap and the units differ.
type DiskCapacityRequest struct {
	Storage    []HeartbeatStoragePath
	Placement  NodePlacement
	Unwritten  map[string]int64 // path -> bytes promised but not yet written, from UnwrittenBytesByPath
	HeadroomGB int64            // free space the path must retain
	WantMB     int64            // the new server's disk limit, in MEGABYTES
}

// DiskCapacityResult explains the decision, so a rejection can name the path and
// the number that caused it instead of a bare "no capacity".
type DiskCapacityResult struct {
	Fits         bool
	Path         string
	FreeGB       int64
	UnwrittenGB  int64 // promised but not yet written by the servers already there
	AvailableGB  int64 // free minus unwritten minus headroom; may be negative
	HeadroomGB   int64
	Undetermined bool // the node reported nothing usable, so this is not a real answer
}

// CheckDiskCapacity decides whether a new server fits on the path the node would
// actually choose.
//
//	unwritten = sum per server of max(0, limit - its usage)   promises still to be honoured
//	available = free - unwritten - headroom                   what a NEW promise may consume
//
// A node reporting no storage at all (offline, or a heartbeat without the field)
// is Undetermined and treated as fitting: refusing every placement because a
// metric is missing would take the fleet out of service on a reporting glitch.
func CheckDiskCapacity(req DiskCapacityRequest) DiskCapacityResult {
	path, freeBytes := PickPlacementPath(req.Storage, req.Placement)
	headroomGB := req.HeadroomGB
	if headroomGB < 0 {
		headroomGB = 0
	}
	res := DiskCapacityResult{Path: path, HeadroomGB: headroomGB}
	if path == "" || freeBytes <= 0 {
		res.Fits = true
		res.Undetermined = true
		return res
	}

	unwritten := req.Unwritten[path]

	headroomBytes := headroomGB * gibiByte
	available := freeBytes - unwritten - headroomBytes

	res.FreeGB = freeBytes / gibiByte
	res.UnwrittenGB = unwritten / gibiByte
	res.AvailableGB = available / gibiByte
	res.Fits = available >= req.WantMB*1024*1024
	return res
}

const gibiByte = int64(1024 * 1024 * 1024)

// Disk enforcement modes. They govern ADMISSION - placing a new server and
// raising an existing limit - not writes that are already in flight.
//
// A path-level "servers may not write another byte" does not exist as a knob:
// the only mechanism that stops a write is the per-server project quota, and
// clamping every server on a breached path down to its current usage would
// freeze live worlds mid-save. That is a deliberate, separate decision, not
// something to switch on behind a settings toggle.
const (
	DiskEnforcementSoft = "soft" // over-promising is allowed, but reported
	DiskEnforcementHard = "hard" // a promise that would breach the headroom is refused

	DiskEnforcementSetting = "placement.disk_enforcement"
	DiskWarnPercentSetting = "placement.disk_warn_percent"
	DiskCritPercentSetting = "placement.disk_critical_percent"

	DefaultDiskWarnPercent = 80
	DefaultDiskCritPercent = 95
)

// DiskEnforcement returns the configured mode, defaulting to soft. Soft is the
// safe default because hard mode can refuse a placement on a fleet whose disk
// limits were never sized against real capacity.
func DiskEnforcement(s store.Store) string {
	if s == nil {
		return DiskEnforcementSoft
	}
	raw, _ := s.GetSetting(DiskEnforcementSetting)
	if strings.TrimSpace(raw) == DiskEnforcementHard {
		return DiskEnforcementHard
	}
	return DiskEnforcementSoft
}

// DiskThresholds returns the warn/critical percentages, clamped into 1..100 and
// ordered so warn never exceeds critical.
func DiskThresholds(s store.Store) (warn, critical int) {
	warn, critical = DefaultDiskWarnPercent, DefaultDiskCritPercent
	if s == nil {
		return warn, critical
	}
	if v := settingPercent(s, DiskWarnPercentSetting); v > 0 {
		warn = v
	}
	if v := settingPercent(s, DiskCritPercentSetting); v > 0 {
		critical = v
	}
	if warn > critical {
		warn = critical
	}
	return warn, critical
}

func settingPercent(s store.Store, key string) int {
	raw, _ := s.GetSetting(key)
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > 100 {
		return 0
	}
	return n
}

// Per-path status values, ordered by severity.
const (
	DiskStatusUnknown  = "unknown"  // the node reported nothing usable
	DiskStatusOK       = "ok"       //
	DiskStatusWarning  = "warning"  // past the warn threshold
	DiskStatusCritical = "critical" // past the critical threshold
	DiskStatusBreached = "breached" // the headroom itself is already gone
)

// DiskPathStatus is one storage path's capacity picture, in the terms an
// operator needs: what is physically free, how much of that is already spoken
// for, and what is actually left to promise.
type DiskPathStatus struct {
	Path             string `json:"path"`
	TotalGB          int64  `json:"totalGb"`
	FreeGB           int64  `json:"freeGb"`
	UsedGB           int64  `json:"usedGb"`
	CommittedGB      int64  `json:"committedGb"`
	UnwrittenGB      int64  `json:"unwrittenGb"`
	AvailableGB      int64  `json:"availableGb"` // may be negative when over-promised
	HeadroomGB       int64  `json:"headroomGb"`
	ProjectedPercent int    `json:"projectedPercent"`
	Status           string `json:"status"`
	QuotaEnforceable bool   `json:"quotaEnforceable"`
}

// PathStatuses computes the per-path picture for a node.
//
// The percentage is PROJECTED, not current: used plus everything still promised,
// over total. Reporting current usage would say a path is 10% full right up to
// the moment twenty servers grow into their limits at once, which is exactly the
// surprise this is meant to remove.
func PathStatuses(storage []HeartbeatStoragePath, committed, unwrittenByPath map[string]int64, headroomGB int64, warnPct, critPct int) []DiskPathStatus {
	out := make([]DiskPathStatus, 0, len(storage))
	for _, sp := range storage {
		unwritten := unwrittenByPath[sp.Path]
		available := sp.FreeBytes - unwritten - headroomGB*gibiByte

		st := DiskPathStatus{
			Path:             sp.Path,
			TotalGB:          sp.TotalBytes / gibiByte,
			FreeGB:           sp.FreeBytes / gibiByte,
			UsedGB:           sp.UsedBytes / gibiByte,
			CommittedGB:      committed[sp.Path] / gibiByte,
			UnwrittenGB:      unwritten / gibiByte,
			AvailableGB:      available / gibiByte,
			HeadroomGB:       headroomGB,
			QuotaEnforceable: sp.QuotaEnforceable,
		}

		switch {
		case sp.TotalBytes <= 0:
			st.Status = DiskStatusUnknown
		default:
			st.ProjectedPercent = int((sp.UsedBytes + unwritten) * 100 / sp.TotalBytes)
			switch {
			case available < 0:
				st.Status = DiskStatusBreached
			case st.ProjectedPercent >= critPct:
				st.Status = DiskStatusCritical
			case st.ProjectedPercent >= warnPct:
				st.Status = DiskStatusWarning
			default:
				st.Status = DiskStatusOK
			}
		}
		out = append(out, st)
	}
	return out
}
