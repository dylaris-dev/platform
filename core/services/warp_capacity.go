package services

import (
	"context"
	"encoding/json"
	"sort"

	"dylaris-pkg/protocol"
)

// gatewayCapacity is the F0 telemetry view F1 needs: which swarm host each warp
// leader runs on (from its component mirror) and each host's free transmit
// capacity in bits/s (from its host aggregate). Both maps omit any entry with no
// usable telemetry; a missing key means "capacity unknown", which the callers
// treat as a fall-back to the historical deterministic order.
type gatewayCapacity struct {
	leaderHost map[string]string // leaderID -> swarm host
	hostFree   map[string]int64  // host -> free bits/s (budget - tx, clamped >= 0)
}

// freeBpsForLeader returns the free capacity of the host a leader runs on and
// whether that capacity is known. Reading a nil map is safe (zero, false), so a
// zero-value gatewayCapacity reports every leader as unknown.
func (c gatewayCapacity) freeBpsForLeader(leaderID string) (int64, bool) {
	host, ok := c.leaderHost[leaderID]
	if !ok {
		return 0, false
	}
	free, ok := c.hostFree[host]
	return free, ok
}

// hostFreeBps converts a host aggregate into free capacity in bits/s.
// The budget is BudgetMbit megabit/s (1 Mbit = 1e6 bits); a host with no
// configured budget (BudgetMbit <= 0, i.e. BANDWIDTH_MBIT unset) has UNKNOWN
// free capacity, not zero. Free is clamped at 0 so an over-budget host never
// reports negative headroom.
//
// Measured against the BUSIER direction, not against transmit alone. Ethernet
// is full duplex, so BudgetMbit is a ceiling for each direction separately -
// which means there are two headrooms, and a leader placed here needs both.
// Transmit is usually the binding one on a host that also runs an edge (players
// download far more than they upload), so this rarely changes the answer; where
// it does, the old reading was the optimistic one.
func hostFreeBps(agg hostAggregate) (int64, bool) {
	if agg.BudgetMbit <= 0 {
		return 0, false
	}
	busier := agg.TxBps
	if agg.RxBps > busier {
		busier = agg.RxBps
	}
	free := int64(agg.BudgetMbit)*1_000_000 - int64(busier)
	if free < 0 {
		free = 0
	}
	return free, true
}

// loadGatewayCapacity reads the F0 Redis mirror for the given warp leaders and
// builds the capacity view. It does two MGETs: the per-leader component mirrors
// to learn each leader's host, then the per-host aggregates to learn each host's
// free capacity. Any leader or host with a missing or unparseable mirror is
// simply absent from the result (= unknown). Best-effort: a Redis error yields
// an empty view, never an error, so placement degrades to the deterministic
// fallback rather than failing enroll.
func (s *WarpService) loadGatewayCapacity(ctx context.Context, leaderIDs []string) gatewayCapacity {
	gc := gatewayCapacity{
		leaderHost: map[string]string{},
		hostFree:   map[string]int64{},
	}
	if len(leaderIDs) == 0 || s.redis == nil {
		return gc
	}

	compKeys := make([]string, len(leaderIDs))
	for i, id := range leaderIDs {
		compKeys[i] = "dylaris:gwbw:component:warp:" + id
	}
	compVals, err := s.redis.MGet(ctx, compKeys...).Result()
	if err != nil {
		return gc
	}
	hostSet := map[string]bool{}
	for i, v := range compVals {
		raw, ok := v.(string)
		if !ok {
			continue
		}
		var stat protocol.GatewayStats
		if json.Unmarshal([]byte(raw), &stat) != nil || stat.Host == "" {
			continue
		}
		gc.leaderHost[leaderIDs[i]] = stat.Host
		hostSet[stat.Host] = true
	}
	if len(hostSet) == 0 {
		return gc
	}

	hosts := make([]string, 0, len(hostSet))
	hostKeys := make([]string, 0, len(hostSet))
	for h := range hostSet {
		hosts = append(hosts, h)
		hostKeys = append(hostKeys, "dylaris:gwbw:host:"+h)
	}
	hostVals, err := s.redis.MGet(ctx, hostKeys...).Result()
	if err != nil {
		return gc
	}
	for i, v := range hostVals {
		raw, ok := v.(string)
		if !ok {
			continue
		}
		var agg hostAggregate
		if json.Unmarshal([]byte(raw), &agg) != nil {
			continue
		}
		if free, known := hostFreeBps(agg); known {
			gc.hostFree[hosts[i]] = free
		}
	}
	return gc
}

// leaderCandidate is one enabled leader of a region with its resolved liveness
// and free-capacity telemetry, ready to be ordered for connect-time placement.
type leaderCandidate struct {
	endpoint string
	id       string
	alive    bool
	freeBps  int64
	known    bool
}

// orderLeadersByFreeCapacity returns the leaders' endpoints ordered for the
// client: alive leaders first; among equal liveness, leaders with KNOWN capacity
// (by most free first) rank ahead of unknown-capacity leaders; every remaining
// tie (both unknown, or equal free) breaks by leader ID. When no leader has
// telemetry this reproduces the historical alive-first-then-ID order.
func orderLeadersByFreeCapacity(cands []leaderCandidate) []string {
	sortLeaderCands(cands)
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.endpoint)
	}
	return out
}

// sortLeaderCands sorts leaders in place by the connect-time placement rule:
// alive first; among equal liveness, known-capacity (most free first) ahead of
// unknown; every remaining tie by leader ID.
func sortLeaderCands(cands []leaderCandidate) {
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.alive != b.alive {
			return a.alive
		}
		if a.known != b.known {
			return a.known
		}
		if a.known && a.freeBps != b.freeBps {
			return a.freeBps > b.freeBps
		}
		return a.id < b.id
	})
}

// moveHomeToFront stably moves the peer's pinned home leader to the front
// whenever it is one of the region's enabled leaders, ALIVE OR NOT. A peer is
// installed on its home only (see WarpService.Enroll), so sending the client
// anywhere else sends it to a leader that does not know it. A home that really
// died is replaced by reconcileHomes, and the client follows that; a home whose
// alive key merely blinked (one failed EXISTS, a Redis restart) keeps working.
// An unknown home (no such enabled row) falls back to the capacity order.
func moveHomeToFront(cands []leaderCandidate, homeLeaderID string) {
	if homeLeaderID == "" {
		return
	}
	idx := -1
	for i := range cands {
		if cands[i].id == homeLeaderID {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return
	}
	home := cands[idx]
	copy(cands[1:idx+1], cands[0:idx])
	cands[0] = home
}

// regionFreeBps sums the free capacity of the DISTINCT hosts backing a region's
// alive leaders. Co-located leaders share one uplink, so each host is counted
// once. known is true if at least one of those hosts reported usable telemetry;
// a region with no telemetry returns (0, false) and the caller falls back to
// peer-count ordering.
func regionFreeBps(gc gatewayCapacity, aliveLeaderIDs []string) (int64, bool) {
	var total int64
	var known bool
	seen := map[string]bool{}
	for _, id := range aliveLeaderIDs {
		host, ok := gc.leaderHost[id]
		if !ok || seen[host] {
			continue
		}
		seen[host] = true
		if free, has := gc.hostFree[host]; has {
			total += free
			known = true
		}
	}
	return total, known
}

// regionCandidate is one enabled region eligible for auto-assignment, with its
// aggregate free capacity (when known), whether any telemetry backed it, and its
// current peer count for the tiebreak.
type regionCandidate struct {
	region    string
	freeBps   int64
	known     bool
	peerCount int
}

// pickFreestRegion returns the region with the MOST aggregate free capacity.
// Regions with known capacity outrank unknown ones; among equal-capacity or
// all-unknown regions the tiebreak is fewest peers, then region name. With no
// telemetry this reproduces the historical least-peer-count-then-name order.
// cands must be non-empty.
func pickFreestRegion(cands []regionCandidate) string {
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.known != b.known {
			return a.known
		}
		if a.known && a.freeBps != b.freeBps {
			return a.freeBps > b.freeBps
		}
		if a.peerCount != b.peerCount {
			return a.peerCount < b.peerCount
		}
		return a.region < b.region
	})
	return cands[0].region
}
