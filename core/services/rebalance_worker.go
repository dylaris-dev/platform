package services

import (
	"context"
	"fmt"
	"log"
	"time"

	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// RebalanceWorker is the leader-gated loop that relieves overloaded nodes by
// migrating eligible servers off them. It only ever ENQUEUES moves onto the
// MigrationOrchestrator (which holds the per-server lock, re-checks 0 players,
// and runs the step machine) — it never moves data itself.
//
// Conservative by design:
//   - Only runs when auto-move is enabled AND gateway routing is active (a move
//     without the gateway breaks the server's address).
//   - Considers only servers with auto_move=true and 0 current players.
//   - Moves at most ONE server per overloaded node per tick (gentle; avoids a
//     thundering herd of simultaneous migrations).
//   - Refuses to move a server unless a target node with real capacity exists —
//     the orchestrator does NOT validate target capacity, so the worker must.
type RebalanceWorker struct {
	store        store.Store
	redis        *redis.Client
	orchestrator *MigrationOrchestrator
	features     *FeatureFlags
	// leader is set by main.go after construction. nil = run unconditionally
	// (single-Core dev mode); non-nil = only act when this Core holds the lease.
	leader leader.Election
}

const (
	// rebalanceTickInterval is how often the worker re-evaluates node load.
	// 60s is slow enough that an in-flight migration (the orchestrator holds a
	// 10m per-server lock) is long finished or still locked-out before the next
	// candidate is considered, and far below any load-change timescale that
	// matters for a game-server fleet.
	rebalanceTickInterval = 60 * time.Second

	// defaultRebalanceThreshold is the load percent above which a node is
	// considered overloaded when placement.rebalance_threshold is unset/invalid.
	// Matches the placement default ceiling.
	defaultRebalanceThreshold = 85
)

func NewRebalanceWorker(s store.Store, r *redis.Client, o *MigrationOrchestrator, f *FeatureFlags) *RebalanceWorker {
	return &RebalanceWorker{
		store:        s,
		redis:        r,
		orchestrator: o,
		features:     f,
	}
}

// SetLeader wires the leader-election gate. The loop only acts on the elected
// Core; followers idle.
func (w *RebalanceWorker) SetLeader(l leader.Election) { w.leader = l }

// Start launches the ticker loop in its own goroutine. Returns immediately.
func (w *RebalanceWorker) Start(ctx context.Context) {
	log.Println("Rebalance Worker started")
	go w.run(ctx)
}

func (w *RebalanceWorker) run(ctx context.Context) {
	ticker := time.NewTicker(rebalanceTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if w.leader != nil && !w.leader.IsLeader() {
			continue
		}
		w.tick(ctx)
	}
}

// gatewayEnabled mirrors the orchestrator/handler check: migration is only safe
// while routing keeps a server's address stable across a node change.
func (w *RebalanceWorker) gatewayEnabled() bool {
	mode, _ := w.store.GetSetting("routing_mode")
	if mode == "" {
		mode = "ip_port"
	}
	return mode == "gateway" || mode == "both"
}

func (w *RebalanceWorker) threshold() int {
	v, _ := w.store.GetSetting("placement.rebalance_threshold")
	var n int
	// Same 50..100 clamp the settings handler enforces; reject out-of-range.
	if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 50 && n <= 100 {
		return n
	}
	return defaultRebalanceThreshold
}

// tick is one evaluation pass. Guard → compute loads → for each overloaded node
// pick one candidate + a target → enqueue. All decisions are logged.
func (w *RebalanceWorker) tick(ctx context.Context) {
	// (a) Guard: stay completely silent when the feature is off or gateway is
	// inactive — no work, no log noise.
	if !w.features.IsAutoMoveEnabled(ctx) || !w.gatewayEnabled() {
		return
	}

	threshold := w.threshold()

	nodes, err := w.store.ListNodes()
	if err != nil {
		logErrf("rebalance", "list nodes failed: %v", err)
		return
	}
	heartbeats := LoadHeartbeats(ctx, w.redis)

	// (c) Compute every online node's load%. Build a candidate-target pool of
	// nodes that are below threshold so we don't recompute it per overloaded node.
	loads := make(map[int]float64, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if n.Status != "online" {
			continue
		}
		allocRAM, allocCPU, _ := w.store.SumAllocatedByNode(n.ID)
		loads[n.ID] = nodeLoadPercent(allocRAM, allocCPU, n.TotalRAMMB, n.TotalCPU, n.RAMOvercommitRatio, n.CPUOvercommitRatio)
	}

	for i := range nodes {
		src := &nodes[i]
		if src.Status != "online" {
			continue
		}
		if loads[src.ID] <= float64(threshold) {
			continue
		}
		w.relieveNode(ctx, src, nodes, loads, heartbeats, threshold)
	}
}

// relieveNode picks one eligible server on the overloaded source node and a
// capacity-checked target, then enqueues the move. At most one move per node
// per tick (logged so the cap is never silent).
func (w *RebalanceWorker) relieveNode(ctx context.Context, src *models.Node, nodes []models.Node, loads map[int]float64, heartbeats map[string]*NodeHeartbeat, threshold int) {
	candidate := w.pickCandidate(ctx, src)
	if candidate == nil {
		log.Printf("rebalance: node %d overloaded (%.0f%% > %d%%) but no eligible 0-player auto_move server to move; skipping",
			src.ID, loads[src.ID], threshold)
		return
	}

	target := w.pickTarget(ctx, candidate, src, nodes, loads, heartbeats, threshold)
	if target == nil {
		log.Printf("rebalance: node %d overloaded (%.0f%%), server %s eligible but no target with capacity; skipping",
			src.ID, loads[src.ID], candidate.UUID)
		return
	}

	if err := w.orchestrator.EnqueueMigration(ctx, candidate.ID, target.ID, "rebalance", "system"); err != nil {
		logErrf("rebalance", "enqueue failed for server %s (node %d -> %d): %v", candidate.UUID, src.ID, target.ID, err)
		return
	}
	log.Printf("rebalance: enqueued server %s (%dMB) node %d (%.0f%%) -> node %d (%.0f%%), threshold %d%% [capped at 1 move/node/tick]",
		candidate.UUID, candidate.Memory, src.ID, loads[src.ID], target.ID, loads[target.ID], threshold)
}

// pickCandidate returns the server on the source node that best relieves it:
// auto_move=true, 0 current players, not already migrating, largest RAM first.
//
// RAM is the heuristic because the allocation model (SumAllocatedByNode) sums
// memory across ALL servers regardless of running state, so moving the largest-
// RAM server drops the measured load the most per move.
func (w *RebalanceWorker) pickCandidate(ctx context.Context, src *models.Node) *models.Server {
	rows, err := w.store.ListServersByNode(src.ID)
	if err != nil {
		logErrf("rebalance", "list servers for node %d failed: %v", src.ID, err)
		return nil
	}

	var best *models.Server
	for i := range rows {
		// ListServersByNode returns a thin row (no memory/auto_move); load the
		// full record to read auto_move + memory.
		srv, err := w.store.GetServerByID(rows[i].ID)
		if err != nil {
			continue
		}
		if !srv.AutoMove {
			continue
		}
		if w.migrationLocked(ctx, srv.UUID) {
			continue
		}
		if playerCountFromStats(ctx, w.redis, srv.UUID) > 0 {
			continue
		}
		if best == nil || srv.Memory > best.Memory {
			best = srv
		}
	}
	return best
}

// sameNodeOwner reports whether two nodes belong to the same party: both to the
// platform (owner nil), or both to the same tenant.
//
// Written as one predicate rather than as a condition at the call site because
// the nil case is the whole point and it is easy to write a comparison that
// only holds in one direction - which is exactly what was here before.
func sameNodeOwner(a, b *models.Node) bool {
	if a.OwnerID == nil || b.OwnerID == nil {
		return a.OwnerID == nil && b.OwnerID == nil
	}
	return *a.OwnerID == *b.OwnerID
}

// migrationLocked reports whether an orchestrator migration is already in flight
// for this server (the per-server lock key exists). Skip such servers so we
// don't double-enqueue.
func (w *RebalanceWorker) migrationLocked(ctx context.Context, serverUUID string) bool {
	key := fmt.Sprintf("dylaris:server:%s:migration", serverUUID)
	n, err := w.redis.Exists(ctx, key).Result()
	if err != nil {
		// On a Redis error treat as locked: safer to skip a move than to risk
		// a concurrent migration.
		return true
	}
	return n > 0
}

// pickTarget selects a capacity-aware destination for the server: online, not
// the source, honoring the source's region/tag constraints, with room for the
// server's RAM+CPU under overcommit, and below the overload threshold. Among
// valid targets the lowest-load node wins. Returns nil when none qualify.
func (w *RebalanceWorker) pickTarget(ctx context.Context, srv *models.Server, src *models.Node, nodes []models.Node, loads map[int]float64, heartbeats map[string]*NodeHeartbeat, threshold int) *models.Node {
	gatewayOn := w.gatewayEnabled()

	var best *models.Node
	bestLoad := 0.0
	for i := range nodes {
		t := &nodes[i]
		if t.ID == src.ID || t.Status != "online" {
			continue
		}
		// External/home nodes only route via gateway+beam; never target one when
		// the platform is ip_port (mirrors placement.go). gatewayOn is true here
		// (we'd have bailed in tick otherwise) but keep the guard explicit.
		if t.IsExternal() && !gatewayOn {
			continue
		}
		// BYON isolation (BC6): a server never crosses an ownership boundary,
		// in EITHER direction. Mirrors canPlaceOnNode / placement.go's
		// OwnerScope guard.
		//
		// Only the tenant-to-elsewhere half was implemented, and the missing
		// half is the worse one: a PLATFORM-owned source skipped the check
		// entirely, and the candidate list is every node there is. A rented
		// server belonging to one customer could therefore be moved, by nobody's
		// decision, onto another customer's own machine - hardware that customer
		// has root on. Nothing downstream would have caught it: the migration
		// orchestrator reads OwnerID only to decide whether to hand over LAN
		// addresses.
		if !sameNodeOwner(src, t) {
			continue
		}
		// Nor across the External boundary. Both kinds of unowned node pass the
		// ownership check above, but an External node sits outside the
		// datacenter behind a WireGuard spoke: moving a server there relocates it
		// out of the building by nobody's decision, and a move in or out needs
		// the object-storage transfer rather than the overlay pull. An admin who
		// wants that moves the server by hand. The tag is self-reported, which
		// can only make a node attract or refuse moves among its own kind.
		if t.IsExternal() != src.IsExternal() {
			continue
		}
		// Keep the server in the same region + match the source's tags so a move
		// doesn't silently relocate a server across a placement boundary.
		if src.Region != "" && !equalRegion(t.Region, src.Region) {
			continue
		}
		if !nodeHasAllTagsCSV(t.Tags, src.Tags) {
			continue
		}
		// Below threshold only — don't move onto a node that's already hot.
		if loads[t.ID] > float64(threshold) {
			continue
		}
		if !nodeHasCapacityFor(w.store, t, heartbeats[t.Token], srv, EffectiveNodePlacement(ctx, w.redis, t.Token)) {
			continue
		}
		if best == nil || loads[t.ID] < bestLoad {
			best = t
			bestLoad = loads[t.ID]
		}
	}
	return best
}
