package services

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-core/pkg/leader"
	"dylaris-core/store"
	"dylaris-pkg/protocol"
)

// --- Pure selection (no I/O; fully table-tested) ---

type warpMove struct {
	Pubkey string `json:"pubkey"`
	From   string `json:"from"`
	To     string `json:"to"`
	TxBps  uint64 `json:"txBps"`
}

type saturatedLeader struct {
	leaderID string
	region   string
	shedBps  int64
	peers    []protocol.PeerBandwidth
}

type moveTarget struct {
	leaderID    string
	region      string
	headroomBps int64
}

type warpMoveInput struct {
	saturated     []saturatedLeader
	targets       []moveTarget
	recentlyMoved map[string]bool
	maxMoves      int
}

// selectWarpMoves picks the minimum set of peer moves that relieves each
// saturated leader onto the freest same-region sibling with headroom. Worst
// leaders (largest shed) are handled first; on each leader the largest peers
// move first (fewest moves). A move requires a same-region target whose
// remaining headroom covers the peer's Tx; that headroom is decremented as moves
// are committed so one target is never oversubscribed within a tick. Peers in
// recentlyMoved are skipped (anti-flap) and the total is capped at maxMoves.
// Pure: same input always yields the same output, no clock, no Redis.
func selectWarpMoves(in warpMoveInput) []warpMove {
	// Working copy of target headroom, keyed by leaderID.
	headroom := make(map[string]int64, len(in.targets))
	targetRegion := make(map[string]string, len(in.targets))
	targetIDs := make([]string, 0, len(in.targets))
	for _, t := range in.targets {
		headroom[t.leaderID] = t.headroomBps
		targetRegion[t.leaderID] = t.region
		targetIDs = append(targetIDs, t.leaderID)
	}

	sat := make([]saturatedLeader, len(in.saturated))
	copy(sat, in.saturated)
	sort.SliceStable(sat, func(i, j int) bool { return sat[i].shedBps > sat[j].shedBps })

	var moves []warpMove
	for _, sl := range sat {
		if in.maxMoves > 0 && len(moves) >= in.maxMoves {
			break
		}
		peers := make([]protocol.PeerBandwidth, len(sl.peers))
		copy(peers, sl.peers)
		sort.SliceStable(peers, func(i, j int) bool { return peers[i].TxBps > peers[j].TxBps })

		remaining := sl.shedBps
		for _, p := range peers {
			if remaining <= 0 {
				break
			}
			if in.maxMoves > 0 && len(moves) >= in.maxMoves {
				break
			}
			if in.recentlyMoved[p.Pubkey] {
				continue
			}
			// Best same-region target: most remaining headroom that still fits.
			best := ""
			var bestHeadroom int64 = -1
			for _, tid := range targetIDs {
				if tid == sl.leaderID || targetRegion[tid] != sl.region {
					continue
				}
				h := headroom[tid]
				if h >= int64(p.TxBps) && h > bestHeadroom {
					best = tid
					bestHeadroom = h
				}
			}
			if best == "" {
				continue // no target can absorb this peer; leave it
			}
			headroom[best] -= int64(p.TxBps)
			remaining -= int64(p.TxBps)
			moves = append(moves, warpMove{Pubkey: p.Pubkey, From: sl.leaderID, To: best, TxBps: p.TxBps})
		}
	}
	return moves
}

// --- Leader-gated worker ---

const (
	// warpRebalanceMaxMovesPerTick keeps a tick conservative so the ~30-60s F0
	// telemetry lag catches up before the next evaluation (mirrors the node
	// RebalanceWorker's "few moves per tick" posture).
	warpRebalanceMaxMovesPerTick = 3

	warpRebalanceDecisionsKey = "dylaris:warp:rebalance:decisions"
	warpRebalanceDecisionsMax = 50
	warpRebalanceLastMovePfx  = "dylaris:warp:rebalance:lastmove:"
)

// warpDecision is one recorded rebalancer decision for the panel feed.
type warpDecision struct {
	TS      int64      `json:"ts"`
	Mode    string     `json:"mode"`
	Applied bool       `json:"applied"`
	Moves   []warpMove `json:"moves"`
	Note    string     `json:"note,omitempty"`
}

// WarpRebalancer is the leader-gated loop that relieves saturated warp leaders
// by pinning individual peers to a freer same-region sibling leader. It reuses
// F2's threshold evaluator to find sustained-hot hosts, the F0 Redis mirror for
// live per-leader/per-host telemetry, and the pure selectWarpMoves to pick the
// actual moves. "off" does nothing; "dry-run" computes and records a decision
// without touching state; "armed" also pins the peer's assigned leader.
type WarpRebalancer struct {
	store    store.Store
	redis    *redis.Client
	features *FeatureFlags
	// leader is set by main.go after construction. nil = run unconditionally
	// (single-Core dev mode); non-nil = only act when this Core holds the lease.
	leader leader.Election
}

func NewWarpRebalancer(s store.Store, r *redis.Client, f *FeatureFlags) *WarpRebalancer {
	return &WarpRebalancer{store: s, redis: r, features: f}
}

// SetLeader wires the leader-election gate. The loop only acts on the elected
// Core; followers idle.
func (w *WarpRebalancer) SetLeader(l leader.Election) { w.leader = l }

// Start launches the evaluation loop. The interval is re-read each cycle so a
// panel change to warp_rebalance_interval_min takes effect without a restart.
func (w *WarpRebalancer) Start(ctx context.Context) {
	log.Println("Warp Rebalancer started")
	go w.run(ctx)
}

func (w *WarpRebalancer) run(ctx context.Context) {
	for {
		interval := time.Duration(w.features.WarpRebalanceIntervalMin(ctx)) * time.Minute
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if w.leader != nil && !w.leader.IsLeader() {
			continue
		}
		w.tick(ctx, time.Now())
	}
}

// gatewayEnabled mirrors RebalanceWorker's check: a warp peer's home leader
// only matters while routing keeps its address stable across a leader change.
func (w *WarpRebalancer) gatewayEnabled() bool {
	mode, _ := w.store.GetSetting("routing_mode")
	if mode == "" {
		mode = "ip_port"
	}
	return mode == "gateway" || mode == "both"
}

// tick is one evaluation pass. It stays completely silent (no work, no writes)
// unless the mode is dry-run/armed and gateway routing is active.
func (w *WarpRebalancer) tick(ctx context.Context, now time.Time) {
	mode := w.features.WarpRebalanceMode(ctx)
	if mode == "off" || !w.gatewayEnabled() {
		return
	}
	pct := w.features.WarpRebalancePct(ctx)
	sustainMin := w.features.WarpRebalanceSustainMin(ctx)
	sustain := time.Duration(sustainMin) * time.Minute

	// 1. Which hosts are sustained over threshold (reuse F2's evaluator).
	rows, err := w.store.GetGatewayBandwidthHistory(now.Add(-sustain), "", "")
	if err != nil {
		logErrf("warp-rebalancer", "history: %v", err)
		return
	}
	alerts := evaluateAlerts(rows, pct, sustain, now)
	hot := map[string]bool{}
	for _, a := range alerts {
		if a.Kind == "host" {
			hot[a.Host] = true
		}
	}
	if len(hot) == 0 {
		return
	}

	// 2. Build the capacity view + per-leader host + PerPeer for all warp leaders.
	leaders, err := w.store.ListWarpLeaders()
	if err != nil {
		logErrf("warp-rebalancer", "leaders: %v", err)
		return
	}
	leaderIDs := make([]string, 0, len(leaders))
	leaderRegion := map[string]string{}
	for _, l := range leaders {
		if !l.Enabled {
			continue
		}
		leaderIDs = append(leaderIDs, l.LeaderID)
		leaderRegion[l.LeaderID] = l.Region
	}
	gc := w.loadCapacity(ctx, leaderIDs) // leaderID -> host map (reads the F0 component mirror)
	hostAgg := w.loadHostAggregates(ctx, gc.hostsOf(leaderIDs))

	// 3. Assemble saturated leaders (on hot hosts, with budget known) + targets.
	var saturated []saturatedLeader
	var targets []moveTarget
	for _, lid := range leaderIDs {
		host := gc.leaderHost[lid]
		agg, ok := hostAgg[host]
		if !ok || agg.BudgetMbit <= 0 {
			continue // unknown capacity -> never a source or target
		}
		budgetBps := int64(agg.BudgetMbit) * 1_000_000
		thresholdBps := budgetBps * int64(pct) / 100
		// A target's headroom is measured against its BUSIER direction: the
		// budget is a per-direction ceiling (full duplex), so a host saturated
		// inbound has no room for more peers even with a quiet uplink.
		busier := int64(agg.TxBps)
		if int64(agg.RxBps) > busier {
			busier = int64(agg.RxBps)
		}
		headroom := thresholdBps - busier
		if headroom < 0 {
			headroom = 0
		}
		targets = append(targets, moveTarget{leaderID: lid, region: leaderRegion[lid], headroomBps: headroom})
		if hot[host] {
			// Shedding is measured in TRANSMIT, because that is the direction
			// the per-peer telemetry can attribute. A host that is hot inbound
			// only therefore has nothing this rebalancer can move, and says so
			// rather than looking like a no-op: relieving it is an operator
			// decision (a bigger uplink, or fewer peers), not a placement one.
			shed := int64(agg.TxBps) - thresholdBps
			if shed <= 0 {
				log.Printf("warp-rebalance: host %s is over threshold inbound (rx %d bps, tx %d bps, threshold %d bps); moving peers cannot relieve that",
					host, agg.RxBps, agg.TxBps, thresholdBps)
				continue
			}
			saturated = append(saturated, saturatedLeader{
				leaderID: lid, region: leaderRegion[lid], shedBps: shed,
				peers: w.loadPeerBandwidth(ctx, lid),
			})
		}
	}
	if len(saturated) == 0 {
		return
	}

	// 4. Anti-flap set + select.
	recent := w.recentlyMoved(ctx, saturated)
	moves := selectWarpMoves(warpMoveInput{
		saturated: saturated, targets: targets,
		recentlyMoved: recent, maxMoves: warpRebalanceMaxMovesPerTick,
	})
	if len(moves) == 0 {
		return
	}
	w.apply(ctx, mode, sustainMin, moves)
}

// apply records the decision and, in armed mode only, writes the new home leader
// and the anti-flap lastmove key for each move. Dry-run records the decision and
// logs but changes nothing.
func (w *WarpRebalancer) apply(ctx context.Context, mode string, sustainMin int, moves []warpMove) {
	applied := mode == "armed"
	if applied {
		for _, m := range moves {
			if err := w.store.SetWarpPeerAssignedLeader(m.Pubkey, m.To); err != nil {
				logErrf("warp-rebalancer", "pin %s -> %s: %v", m.Pubkey, m.To, err)
				continue
			}
			// A peer lives on its home leader only (see WarpService.Enroll), so
			// the move is also a handover: the new home gets it, the old one
			// drops it. The client follows on its next assignment poll.
			if p, err := w.store.GetWarpPeerByPubkey(m.Pubkey); err == nil && p != nil {
				w.push(ctx, m.To, addPeerCmd(p.Pubkey, p.WGIP))
				w.push(ctx, m.From, removePeerCmd(p.Pubkey))
			} else {
				logErrf("warp-rebalancer", "handover %s: %v", m.Pubkey, err)
			}
			w.redis.Set(ctx, warpRebalanceLastMovePfx+m.Pubkey, "1", time.Duration(sustainMin)*time.Minute)
			log.Printf("warp-rebalance: moved %s from %s to %s (%d bps)", m.Pubkey, m.From, m.To, m.TxBps)
		}
	} else {
		log.Printf("warp-rebalance: dry-run would move %d peer(s)", len(moves))
	}
	dec := warpDecision{TS: time.Now().Unix(), Mode: mode, Applied: applied, Moves: moves}
	if b, err := json.Marshal(dec); err == nil {
		w.redis.LPush(ctx, warpRebalanceDecisionsKey, b)
		w.redis.LTrim(ctx, warpRebalanceDecisionsKey, 0, warpRebalanceDecisionsMax-1)
	}
}

func (w *WarpRebalancer) push(ctx context.Context, leaderID string, cmd map[string]interface{}) {
	b, err := json.Marshal(cmd)
	if err != nil {
		return
	}
	if err := w.redis.RPush(ctx, warpLeaderQueueKey(leaderID), b).Err(); err != nil {
		logErrf("warp-rebalancer", "push to %s: %v", leaderID, err)
	}
}

// recentlyMoved builds the anti-flap set: pubkeys of peers on saturated leaders
// that were moved within the sustain window (lastmove key still alive).
func (w *WarpRebalancer) recentlyMoved(ctx context.Context, saturated []saturatedLeader) map[string]bool {
	out := map[string]bool{}
	for _, sl := range saturated {
		for _, p := range sl.peers {
			if n, _ := w.redis.Exists(ctx, warpRebalanceLastMovePfx+p.Pubkey).Result(); n > 0 {
				out[p.Pubkey] = true
			}
		}
	}
	return out
}

// loadPeerBandwidth reads leader lid's live PerPeer list from the F0 component
// mirror. Best-effort: a missing/stale mirror yields no peers (that leader simply
// cannot be relieved this tick).
func (w *WarpRebalancer) loadPeerBandwidth(ctx context.Context, lid string) []protocol.PeerBandwidth {
	raw, err := w.redis.Get(ctx, "dylaris:gwbw:component:warp:"+lid).Result()
	if err != nil {
		return nil
	}
	var stat protocol.GatewayStats
	if json.Unmarshal([]byte(raw), &stat) != nil {
		return nil
	}
	return stat.PerPeer
}

// rebalCapacity maps each warp leader to its swarm host, read from the F0
// component mirror. Deliberately separate from WarpService's gatewayCapacity
// (an unexported method on a different service) rather than reaching across
// services for it.
type rebalCapacity struct {
	leaderHost map[string]string
}

// hostsOf returns the distinct hosts backing leaderIDs, in first-seen order.
func (c rebalCapacity) hostsOf(leaderIDs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range leaderIDs {
		if h, ok := c.leaderHost[id]; ok && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// loadCapacity maps each warp leader to its host via the F0 component mirror.
func (w *WarpRebalancer) loadCapacity(ctx context.Context, leaderIDs []string) rebalCapacity {
	c := rebalCapacity{leaderHost: map[string]string{}}
	for _, id := range leaderIDs {
		raw, err := w.redis.Get(ctx, "dylaris:gwbw:component:warp:"+id).Result()
		if err != nil {
			continue
		}
		var stat protocol.GatewayStats
		if json.Unmarshal([]byte(raw), &stat) == nil && stat.Host != "" {
			c.leaderHost[id] = stat.Host
		}
	}
	return c
}

// loadHostAggregates reads each host's F0 aggregate (tx + budget) for headroom
// and shed math. hostAggregate is defined in gateway_bandwidth_consumer.go.
func (w *WarpRebalancer) loadHostAggregates(ctx context.Context, hosts []string) map[string]hostAggregate {
	out := map[string]hostAggregate{}
	for _, h := range hosts {
		raw, err := w.redis.Get(ctx, "dylaris:gwbw:host:"+h).Result()
		if err != nil {
			continue
		}
		var agg hostAggregate
		if json.Unmarshal([]byte(raw), &agg) == nil {
			out[h] = agg
		}
	}
	return out
}
