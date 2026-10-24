package services

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// CPUCore / CPUTopology mirror the JSON the node publishes to
// dylaris:node:{token}:cpu. Type is "P" | "E" | "standard".
type CPUCore struct {
	ID          int    `json:"id"`
	Type        string `json:"type"`
	Sibling     int    `json:"sibling"`
	MaxClockMHz int    `json:"maxClockMHz"` // display only; never part of TopologySignature
	CacheGroup  int    `json:"cacheGroup"`  // display only; never part of TopologySignature
	L3KB        int    `json:"l3KB"`        // display only; never part of TopologySignature
}

type CPUTopology struct {
	LogicalCount  int       `json:"logicalCount"`
	PhysicalCount int       `json:"physicalCount"`
	Hybrid        bool      `json:"hybrid"`
	Cores         []CPUCore `json:"cores"`
	ScannedAt     int64     `json:"scannedAt"`
}

const cpuReserveHostCores = 1 // keep at least this many cores free for the host

// CPUPinningService reads node CPU topology from Redis and computes auto cpusets.
type CPUPinningService struct {
	redis *redis.Client
	store store.Store
}

func NewCPUPinningService(rdb *redis.Client, st store.Store) *CPUPinningService {
	return &CPUPinningService{redis: rdb, store: st}
}

// GetNodeTopology reads the topology a node published. Returns (nil, nil) when the
// node has not reported one (key missing) so callers can fall back gracefully.
func (s *CPUPinningService) GetNodeTopology(ctx context.Context, nodeToken string) (*CPUTopology, error) {
	raw, err := s.redis.Get(ctx, fmt.Sprintf("dylaris:node:%s:cpu", nodeToken)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var topo CPUTopology
	if err := json.Unmarshal([]byte(raw), &topo); err != nil {
		return nil, err
	}
	return &topo, nil
}

// NodeCoreLoad returns, per logical core id, how many servers on the node are
// currently pinned to it. excludeServerID is skipped (the server being changed).
func (s *CPUPinningService) NodeCoreLoad(nodeID, excludeServerID int) (map[int]int, error) {
	cpusets, err := s.store.ListServerCpusetsByNode(nodeID)
	if err != nil {
		return nil, err
	}
	load := map[int]int{}
	for serverID, cpuset := range cpusets {
		if serverID == excludeServerID {
			continue
		}
		for _, id := range parseCpusetList(cpuset) {
			load[id]++
		}
	}
	return load, nil
}

// AutoCpuset computes a spread, P-preferred cpuset for a server in auto mode,
// restricted to the node's allowed core pool (nodeCpuset; empty = all cores).
// Returns "" when no topology is available (caller falls back to the node default).
func (s *CPUPinningService) AutoCpuset(ctx context.Context, nodeToken string, nodeID, excludeServerID int, cpuLimit float64, nodeCpuset string) (string, error) {
	topo, err := s.GetNodeTopology(ctx, nodeToken)
	if err != nil {
		return "", err
	}
	if topo == nil {
		return "", nil
	}
	load, err := s.NodeCoreLoad(nodeID, excludeServerID)
	if err != nil {
		return "", err
	}
	budget := int(math.Ceil(cpuLimit))
	if budget < 1 {
		budget = 1
	}
	allowed := allowedCoreSet(nodeCpuset)
	// When the admin has carved out a node pool, they already reserved host cores
	// by limiting the pool, so do not reserve again inside it.
	reserve := cpuReserveHostCores
	if len(allowed) > 0 {
		reserve = 0
	}
	return computeAutoCpuset(topo, budget, reserve, load, allowed), nil
}

// PinTooSmall reports a manual pin with fewer cores than the CPU limit grants.
// The limit is the container's CPU time (NanoCPUs), the pin only where it may
// run; Docker applies both, so 4 cores of time on 3 pinned cores is 3 cores.
// need is the cores the limit asks for (rounded up); a limit of 0 (none) or an
// empty pin never conflicts.
func PinTooSmall(cpuLimit float64, cpuset string) (have, need int, tooSmall bool) {
	have = len(parseCpusetList(cpuset))
	if cpuLimit <= 0 || have == 0 {
		return have, 0, false
	}
	need = int(math.Ceil(cpuLimit - 1e-9))
	return have, need, have < need
}

// ValidateCpuset checks a manual cpuset against the node topology AND the node's
// allowed core pool: every listed core must exist and (when a pool is set) be
// within it. If the topology is unavailable it cannot validate, so it accepts the
// value (best-effort) rather than blocking the operator.
func (s *CPUPinningService) ValidateCpuset(ctx context.Context, nodeToken, cpuset, nodeCpuset string) error {
	ids := parseCpusetList(cpuset)
	if len(ids) == 0 {
		return fmt.Errorf("cpuset is empty")
	}
	allowed := allowedCoreSet(nodeCpuset)
	topo, err := s.GetNodeTopology(ctx, nodeToken)
	if err != nil || topo == nil {
		// Cannot check existence, but still enforce the node pool if one is set.
		if len(allowed) > 0 {
			for _, id := range ids {
				if !allowed[id] {
					return fmt.Errorf("core %d is outside this node's allowed pool (%s)", id, nodeCpuset)
				}
			}
		}
		return nil
	}
	valid := map[int]bool{}
	for _, c := range topo.Cores {
		valid[c.ID] = true
	}
	for _, id := range ids {
		if !valid[id] {
			return fmt.Errorf("core %d does not exist on this node (logical cores 0-%d)", id, topo.LogicalCount-1)
		}
		if len(allowed) > 0 && !allowed[id] {
			return fmt.Errorf("core %d is outside this node's allowed pool (%s)", id, nodeCpuset)
		}
	}
	return nil
}

// allowedCoreSet parses a node cpuset into a presence set. Empty cpuset = nil
// (no restriction; all cores allowed).
func allowedCoreSet(nodeCpuset string) map[int]bool {
	ids := parseCpusetList(nodeCpuset)
	if len(ids) == 0 {
		return nil
	}
	set := make(map[int]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// TopologySignature is a stable fingerprint of the host CPU layout, EXCLUDING the
// scan timestamp, so it only changes when the actual hardware (core count / ids /
// P-E classification) changes. Used to detect a hardware change across restarts.
func TopologySignature(t *CPUTopology) string {
	if t == nil {
		return ""
	}
	parts := make([]string, 0, len(t.Cores)+1)
	parts = append(parts, fmt.Sprintf("%d/%d/%t", t.LogicalCount, t.PhysicalCount, t.Hybrid))
	for _, c := range t.Cores {
		parts = append(parts, fmt.Sprintf("%d:%s", c.ID, c.Type))
	}
	return strings.Join(parts, ",")
}

// computeAutoCpuset picks `budget` cores, preferring performance cores, then
// least-loaded, then lowest id, while reserving reserveHostCores for the host.
// allowed (when non-nil) restricts the candidate cores to the node's pool.
// Pure + deterministic.
func computeAutoCpuset(topo *CPUTopology, budget, reserveHostCores int, load map[int]int, allowed map[int]bool) string {
	if topo == nil || len(topo.Cores) == 0 || budget <= 0 {
		return ""
	}

	// Candidate cores: the node pool when set, otherwise all cores.
	cores := make([]CPUCore, 0, len(topo.Cores))
	for _, c := range topo.Cores {
		if allowed == nil || allowed[c.ID] {
			cores = append(cores, c)
		}
	}
	if len(cores) == 0 {
		return ""
	}

	maxAssignable := len(cores) - reserveHostCores
	if maxAssignable < 1 {
		maxAssignable = 1
	}
	if budget > maxAssignable {
		budget = maxAssignable
	}

	sort.SliceStable(cores, func(i, j int) bool {
		ri, rj := coreTypeRank(cores[i].Type), coreTypeRank(cores[j].Type)
		if ri != rj {
			return ri < rj
		}
		li, lj := load[cores[i].ID], load[cores[j].ID]
		if li != lj {
			return li < lj
		}
		return cores[i].ID < cores[j].ID
	})

	chosen := make([]int, 0, budget)
	for i := 0; i < budget && i < len(cores); i++ {
		chosen = append(chosen, cores[i].ID)
	}
	sort.Ints(chosen)
	return compactCpuset(chosen)
}

func coreTypeRank(t string) int {
	switch t {
	case "P":
		return 0
	case "E":
		return 2
	default: // "standard" / unknown
		return 1
	}
}

// compactCpuset renders sorted ids as a cpuset string, collapsing runs:
// [0 1 2 3 8] -> "0-3,8".
func compactCpuset(ids []int) string {
	if len(ids) == 0 {
		return ""
	}
	var parts []string
	start, prev := ids[0], ids[0]
	flush := func() {
		if start == prev {
			parts = append(parts, strconv.Itoa(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}
	}
	for _, id := range ids[1:] {
		if id == prev+1 {
			prev = id
			continue
		}
		flush()
		start, prev = id, id
	}
	flush()
	return strings.Join(parts, ",")
}

// parseCpusetList expands "0-3,8" into [0 1 2 3 8]. Invalid tokens are skipped.
func parseCpusetList(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '-'); i >= 0 {
			lo, err1 := strconv.Atoi(strings.TrimSpace(part[:i]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(part[i+1:]))
			if err1 != nil || err2 != nil || hi < lo {
				continue
			}
			for n := lo; n <= hi; n++ {
				out = append(out, n)
			}
		} else if n, err := strconv.Atoi(part); err == nil {
			out = append(out, n)
		}
	}
	return out
}
