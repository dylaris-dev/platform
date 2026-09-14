package services

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// rebalanceFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods RebalanceWorker touches
// (via pickTarget/threshold/pickCandidate) are overridden. Any other call
// would panic - no test here makes one.
type rebalanceFakeStore struct {
	store.Store

	settings map[string]string

	// SumAllocatedByNode, keyed by node ID.
	allocRAM map[int]int64
	allocCPU map[int]float64

	// ListServersByNode / GetServerByID, for pickCandidate.
	serversByNode        map[int][]models.Server
	serversByID          map[int]*models.Server
	listServersByNodeErr error
}

func (f *rebalanceFakeStore) GetSetting(key string) (string, error) {
	return f.settings[key], nil
}

func (f *rebalanceFakeStore) SumAllocatedByNode(nodeID int) (int64, float64, error) {
	return f.allocRAM[nodeID], f.allocCPU[nodeID], nil
}

func (f *rebalanceFakeStore) ListServersByNode(nodeID int) ([]models.Server, error) {
	return f.serversByNode[nodeID], f.listServersByNodeErr
}

func (f *rebalanceFakeStore) GetServerByID(id int) (*models.Server, error) {
	if s, ok := f.serversByID[id]; ok {
		return s, nil
	}
	return nil, errors.New("server not found")
}

func strPtr(s string) *string { return &s }

// seedPlayerStats writes one stats-buffer entry reporting the given player
// count, matching the shape playerCountFromStats reads (see
// migration_orchestrator.go).
func seedPlayerStats(t *testing.T, rdb *redis.Client, uuid string, players int) {
	t.Helper()
	stream := fmt.Sprintf("dylaris:server:%s:stats:buffer", uuid)
	data := fmt.Sprintf(`{"players": %d}`, players)
	if _, err := rdb.XAdd(context.Background(), &redis.XAddArgs{
		Stream: stream,
		Values: map[string]interface{}{"data": data},
	}).Result(); err != nil {
		t.Fatalf("seed player stats on %s: %v", stream, err)
	}
}

// --- threshold() ---

func TestRebalanceWorker_Threshold(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want int
	}{
		{"unset -> default", "", 85},
		{"lower boundary 50 accepted", "50", 50},
		{"upper boundary 100 accepted", "100", 100},
		{"below lower boundary rejected", "49", 85},
		{"above upper boundary rejected", "101", 85},
		{"garbage rejected", "not-a-number", 85},
		{"valid mid value", "90", 90},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &rebalanceFakeStore{settings: map[string]string{"placement.rebalance_threshold": c.val}}
			w := &RebalanceWorker{store: fs}
			if got := w.threshold(); got != c.want {
				t.Errorf("threshold() = %d, want %d", got, c.want)
			}
		})
	}
}

// --- migrationLocked() ---

func TestRebalanceWorker_MigrationLocked(t *testing.T) {
	t.Run("lock key present -> true", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()
		w := &RebalanceWorker{redis: rdb}
		if err := rdb.Set(ctx, "dylaris:server:srv-1:migration", "user-1", 0).Err(); err != nil {
			t.Fatalf("seed lock key: %v", err)
		}
		if !w.migrationLocked(ctx, "srv-1") {
			t.Error("expected migrationLocked=true when the lock key exists")
		}
	})

	t.Run("lock key absent -> false", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		w := &RebalanceWorker{redis: rdb}
		if w.migrationLocked(context.Background(), "srv-2") {
			t.Error("expected migrationLocked=false when the lock key is absent")
		}
	})

	t.Run("redis error -> fail-safe true", func(t *testing.T) {
		mr, err := miniredis.Run()
		if err != nil {
			t.Fatalf("miniredis: %v", err)
		}
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		w := &RebalanceWorker{redis: rdb}

		// The error has to come from the CLIENT, not from the address. Closing
		// only miniredis leaves the test asserting that nothing else binds the
		// freed port for the duration of one call - an assumption about the
		// machine, not about the code. It held locally over 20 runs and failed
		// on CI (run 30894163959), where this package leaks background workers
		// that keep dialling. A closed client returns redis.ErrClosed for every
		// command, with no network involved at all.
		mr.Close()
		if err := rdb.Close(); err != nil {
			t.Fatalf("close client: %v", err)
		}

		if !w.migrationLocked(context.Background(), "srv-3") {
			t.Error("expected migrationLocked=true (fail-safe) on a redis error")
		}
	})
}

// --- pickTarget() ---
//
// Constructed per the brief: only the store field is set on RebalanceWorker;
// pickTarget must not touch orchestrator/features/redis/leader (all left nil
// here) - if it did, these tests would panic on a nil-pointer method call.

func TestRebalanceWorker_PickTarget(t *testing.T) {
	t.Run("excludes the source node itself", func(t *testing.T) {
		fs := &rebalanceFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
		w := &RebalanceWorker{store: fs}
		src := &models.Node{ID: 1, Status: "online"}
		nodes := []models.Node{*src}
		srv := &models.Server{ID: 100, UUID: "srv-1", Memory: 1024}
		loads := map[int]float64{1: 10}

		if got := w.pickTarget(context.Background(), srv, src, nodes, loads, nil, 85); got != nil {
			t.Errorf("expected nil (only candidate is the source itself), got node %d", got.ID)
		}
	})

	t.Run("region mismatch rejected", func(t *testing.T) {
		fs := &rebalanceFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
		w := &RebalanceWorker{store: fs}
		src := &models.Node{ID: 1, Status: "online", Region: "eu"}
		other := models.Node{ID: 2, Status: "online", Region: "us", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		nodes := []models.Node{*src, other}
		srv := &models.Server{ID: 100, UUID: "srv-1", Memory: 1024}
		loads := map[int]float64{2: 10}

		if got := w.pickTarget(context.Background(), srv, src, nodes, loads, nil, 85); got != nil {
			t.Errorf("expected nil (region mismatch), got node %d", got.ID)
		}
	})

	t.Run("tag mismatch rejected", func(t *testing.T) {
		fs := &rebalanceFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
		w := &RebalanceWorker{store: fs}
		src := &models.Node{ID: 1, Status: "online", Tags: "ssd,premium"}
		other := models.Node{ID: 2, Status: "online", Tags: "hdd", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		nodes := []models.Node{*src, other}
		srv := &models.Server{ID: 100, UUID: "srv-1", Memory: 1024}
		loads := map[int]float64{2: 10}

		if got := w.pickTarget(context.Background(), srv, src, nodes, loads, nil, 85); got != nil {
			t.Errorf("expected nil (missing required tag), got node %d", got.ID)
		}
	})

	// Pins the exact source behavior: the threshold check is `loads[t.ID] >
	// threshold` (strictly greater), so a candidate AT the threshold is
	// accepted, not rejected. The task brief's guide said ">=" - the real
	// source uses ">"; this test pins the actual comparator.
	t.Run("threshold: strictly-over rejected, at-threshold accepted", func(t *testing.T) {
		fs := &rebalanceFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
		w := &RebalanceWorker{store: fs}
		src := &models.Node{ID: 1, Status: "online"}
		over := models.Node{ID: 2, Status: "online", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		atThreshold := models.Node{ID: 3, Status: "online", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		nodes := []models.Node{*src, over, atThreshold}
		srv := &models.Server{ID: 100, UUID: "srv-1", Memory: 1024}
		loads := map[int]float64{2: 86, 3: 85}

		got := w.pickTarget(context.Background(), srv, src, nodes, loads, nil, 85)
		if got == nil || got.ID != 3 {
			t.Fatalf("expected node 3 (load==threshold accepted, load>threshold rejected), got %+v", got)
		}
	})

	t.Run("capacity: insufficient RAM rejected, sufficient wins", func(t *testing.T) {
		fs := &rebalanceFakeStore{
			settings: map[string]string{"routing_mode": "gateway"},
			allocRAM: map[int]int64{2: 900, 3: 0},
		}
		w := &RebalanceWorker{store: fs}
		src := &models.Node{ID: 1, Status: "online"}
		tight := models.Node{ID: 2, Status: "online", TotalRAMMB: 1024, RAMOvercommitRatio: 1.0}
		roomy := models.Node{ID: 3, Status: "online", TotalRAMMB: 4096, RAMOvercommitRatio: 1.0}
		nodes := []models.Node{*src, tight, roomy}
		srv := &models.Server{ID: 100, UUID: "srv-1", Memory: 1024}
		loads := map[int]float64{2: 10, 3: 10}

		got := w.pickTarget(context.Background(), srv, src, nodes, loads, nil, 85)
		if got == nil || got.ID != 3 {
			t.Fatalf("expected node 3 (only one with capacity for 1024MB), got %+v", got)
		}
	})

	t.Run("among multiple eligible, lowest load wins", func(t *testing.T) {
		fs := &rebalanceFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
		w := &RebalanceWorker{store: fs}
		src := &models.Node{ID: 1, Status: "online"}
		n2 := models.Node{ID: 2, Status: "online", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		n3 := models.Node{ID: 3, Status: "online", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		n4 := models.Node{ID: 4, Status: "online", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		nodes := []models.Node{*src, n2, n3, n4}
		srv := &models.Server{ID: 100, UUID: "srv-1", Memory: 1024}
		loads := map[int]float64{2: 50, 3: 30, 4: 70}

		got := w.pickTarget(context.Background(), srv, src, nodes, loads, nil, 85)
		if got == nil || got.ID != 3 {
			t.Fatalf("expected node 3 (lowest load 30), got %+v", got)
		}
	})

	t.Run("no eligible target across mixed filters -> nil", func(t *testing.T) {
		fs := &rebalanceFakeStore{
			settings: map[string]string{"routing_mode": "gateway"},
			allocRAM: map[int]int64{4: 4000},
		}
		w := &RebalanceWorker{store: fs}
		src := &models.Node{ID: 1, Status: "online", Region: "eu", Tags: "ssd"}
		offline := models.Node{ID: 2, Status: "offline", Region: "eu", Tags: "ssd", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		wrongRegion := models.Node{ID: 3, Status: "online", Region: "us", Tags: "ssd", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		noCapacity := models.Node{ID: 4, Status: "online", Region: "eu", Tags: "ssd", TotalRAMMB: 4096, RAMOvercommitRatio: 1.0}
		nodes := []models.Node{*src, offline, wrongRegion, noCapacity}
		srv := &models.Server{ID: 100, UUID: "srv-1", Memory: 1024}
		loads := map[int]float64{3: 10, 4: 10}

		if got := w.pickTarget(context.Background(), srv, src, nodes, loads, nil, 85); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	// pickTarget's own IsExternal guard only fires when gatewayOn is false
	// (`t.IsExternal() && !gatewayOn`); when gateway routing is on, an
	// external node is not excluded by this check. Pinning both branches.
	t.Run("external node excluded only when gateway routing is off", func(t *testing.T) {
		src := &models.Node{ID: 1, Status: "online"}
		ext := models.Node{ID: 2, Status: "online", Tags: "external", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		nodes := []models.Node{*src, ext}
		srv := &models.Server{ID: 100, UUID: "srv-1", Memory: 1024}
		loads := map[int]float64{2: 10}

		t.Run("routing_mode=ip_port (gateway off)", func(t *testing.T) {
			fs := &rebalanceFakeStore{settings: map[string]string{"routing_mode": ""}}
			w := &RebalanceWorker{store: fs}
			if got := w.pickTarget(context.Background(), srv, src, nodes, loads, nil, 85); got != nil {
				t.Errorf("expected nil (external excluded when routing is ip_port), got %+v", got)
			}
		})

		// An External source, so the External boundary (pinned below) does not
		// exclude the target and only the routing mode decides.
		extSrc := &models.Node{ID: 1, Status: "online", Tags: "external"}
		extNodes := []models.Node{*extSrc, ext}

		t.Run("routing_mode=gateway (gateway on)", func(t *testing.T) {
			fs := &rebalanceFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
			w := &RebalanceWorker{store: fs}
			got := w.pickTarget(context.Background(), srv, extSrc, extNodes, loads, nil, 85)
			if got == nil || got.ID != 2 {
				t.Errorf("expected node 2 (external only excluded when gateway is off), got %+v", got)
			}
		})
	})

	// Both kinds of unowned node pass the ownership rule, so the External
	// boundary is its own check: the rebalancer never moves a server out of the
	// datacenter onto an External node, or back in. The node on the wrong side
	// is the least loaded each time, so only the boundary can exclude it.
	t.Run("never crosses the External boundary", func(t *testing.T) {
		fs := &rebalanceFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
		w := &RebalanceWorker{store: fs}
		srv := &models.Server{ID: 100, UUID: "srv-1", Memory: 1024}
		inside := models.Node{ID: 2, Status: "online", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		outside := models.Node{ID: 3, Status: "online", Tags: "external", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}

		platformSrc := &models.Node{ID: 1, Status: "online"}
		got := w.pickTarget(context.Background(), srv, platformSrc, []models.Node{*platformSrc, inside, outside}, map[int]float64{2: 50, 3: 5}, nil, 85)
		if got == nil || got.ID != 2 {
			t.Errorf("platform source: expected node 2 (inside), got %+v", got)
		}
		externalSrc := &models.Node{ID: 1, Status: "online", Tags: "external"}
		got = w.pickTarget(context.Background(), srv, externalSrc, []models.Node{*externalSrc, inside, outside}, map[int]float64{2: 5, 3: 50}, nil, 85)
		if got == nil || got.ID != 3 {
			t.Errorf("External source: expected node 3 (another External node), got %+v", got)
		}
		if got := w.pickTarget(context.Background(), srv, platformSrc, []models.Node{*platformSrc, outside}, map[int]float64{3: 5}, nil, 85); got != nil {
			t.Errorf("platform source with only an External node: expected no target, got node %d", got.ID)
		}
	})

	// BYON isolation (BC6): a tenant-owned source only ever lands on that
	// same tenant's nodes, never a platform node, never a different tenant.
	t.Run("BYON owner-scope: source tenant only lands on its own nodes", func(t *testing.T) {
		fs := &rebalanceFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
		w := &RebalanceWorker{store: fs}
		tenantA := strPtr("tenant-a")
		tenantB := strPtr("tenant-b")
		src := &models.Node{ID: 1, Status: "online", OwnerID: tenantA}
		platformNode := models.Node{ID: 2, Status: "online", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0} // OwnerID nil
		otherTenant := models.Node{ID: 3, Status: "online", OwnerID: tenantB, TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		sameTenant := models.Node{ID: 4, Status: "online", OwnerID: tenantA, TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		nodes := []models.Node{*src, platformNode, otherTenant, sameTenant}
		srv := &models.Server{ID: 100, UUID: "srv-1", Memory: 1024}
		loads := map[int]float64{2: 10, 3: 10, 4: 10}

		got := w.pickTarget(context.Background(), srv, src, nodes, loads, nil, 85)
		if got == nil || got.ID != 4 {
			t.Fatalf("expected node 4 (same tenant), got %+v", got)
		}
	})

	// The other half of the same rule, and the one that was missing. A
	// platform-owned source skipped the ownership check completely, so a rented
	// server belonging to one customer could be moved onto ANOTHER customer's
	// own machine - by no one's decision, and onto hardware that customer has
	// root on.
	t.Run("platform source never lands on a tenant node", func(t *testing.T) {
		fs := &rebalanceFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
		w := &RebalanceWorker{store: fs}
		tenantA := strPtr("tenant-a")
		src := &models.Node{ID: 1, Status: "online"} // OwnerID nil: a platform node
		// Deliberately the LEAST loaded, so it wins on every other criterion and
		// only ownership can exclude it. A test where the right answer also
		// happens to be the cheapest proves nothing.
		tenantNode := models.Node{ID: 2, Status: "online", OwnerID: tenantA, TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		platformNode := models.Node{ID: 3, Status: "online", TotalRAMMB: 8192, RAMOvercommitRatio: 1.0}
		nodes := []models.Node{*src, tenantNode, platformNode}
		srv := &models.Server{ID: 100, UUID: "srv-1", Memory: 1024}
		loads := map[int]float64{2: 5, 3: 50}

		got := w.pickTarget(context.Background(), srv, src, nodes, loads, nil, 85)
		if got == nil || got.ID != 3 {
			t.Fatalf("expected node 3 (the platform node), got %+v", got)
		}
	})

	// With no platform node to fall back on, the answer is "no target", not
	// "the tenant's machine will do".
	t.Run("platform source with only tenant nodes has nowhere to go", func(t *testing.T) {
		fs := &rebalanceFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
		w := &RebalanceWorker{store: fs}
		tenantA := strPtr("tenant-a")
		tenantB := strPtr("tenant-b")
		src := &models.Node{ID: 1, Status: "online"}
		nodes := []models.Node{
			*src,
			{ID: 2, Status: "online", OwnerID: tenantA, TotalRAMMB: 8192, RAMOvercommitRatio: 1.0},
			{ID: 3, Status: "online", OwnerID: tenantB, TotalRAMMB: 8192, RAMOvercommitRatio: 1.0},
		}
		srv := &models.Server{ID: 100, UUID: "srv-1", Memory: 1024}
		loads := map[int]float64{2: 5, 3: 5}

		if got := w.pickTarget(context.Background(), srv, src, nodes, loads, nil, 85); got != nil {
			t.Fatalf("expected no target, got node %d", got.ID)
		}
	})
}

// --- pickCandidate() ---

func TestRebalanceWorker_PickCandidate(t *testing.T) {
	t.Run("selects auto_move, unlocked, 0-player, largest-RAM server", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()

		src := &models.Node{ID: 1}
		fs := &rebalanceFakeStore{
			serversByNode: map[int][]models.Server{
				1: {{ID: 10}, {ID: 11}, {ID: 12}, {ID: 13}, {ID: 14}},
			},
			serversByID: map[int]*models.Server{
				10: {ID: 10, UUID: "srv-not-automove", AutoMove: false, Memory: 8192},
				11: {ID: 11, UUID: "srv-locked", AutoMove: true, Memory: 8192},
				12: {ID: 12, UUID: "srv-has-players", AutoMove: true, Memory: 8192},
				13: {ID: 13, UUID: "srv-eligible-small", AutoMove: true, Memory: 1024},
				14: {ID: 14, UUID: "srv-eligible-large", AutoMove: true, Memory: 4096},
			},
		}
		if err := rdb.Set(ctx, "dylaris:server:srv-locked:migration", "x", 0).Err(); err != nil {
			t.Fatalf("seed lock: %v", err)
		}
		seedPlayerStats(t, rdb, "srv-has-players", 3)

		w := &RebalanceWorker{store: fs, redis: rdb}
		got := w.pickCandidate(ctx, src)
		if got == nil {
			t.Fatal("expected a candidate, got nil")
		}
		if got.UUID != "srv-eligible-large" {
			t.Errorf("pickCandidate = %q, want srv-eligible-large (largest RAM among eligible)", got.UUID)
		}
	})

	t.Run("no eligible server -> nil", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		src := &models.Node{ID: 2}
		fs := &rebalanceFakeStore{
			serversByNode: map[int][]models.Server{2: {{ID: 20}}},
			serversByID:   map[int]*models.Server{20: {ID: 20, UUID: "srv-x", AutoMove: false}},
		}
		w := &RebalanceWorker{store: fs, redis: rdb}
		if got := w.pickCandidate(context.Background(), src); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("ListServersByNode error -> nil", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		src := &models.Node{ID: 3}
		fs := &rebalanceFakeStore{listServersByNodeErr: errors.New("db down")}
		w := &RebalanceWorker{store: fs, redis: rdb}
		if got := w.pickCandidate(context.Background(), src); got != nil {
			t.Errorf("expected nil on list error, got %+v", got)
		}
	})
}
