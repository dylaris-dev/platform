package services

import (
	"context"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// routingFakeStore returns a fixed server list. GetServerByUUID is never reached
// in these tests - redeployServer panics before it, which is the point.
type routingFakeStore struct {
	store.Store
	servers []models.Server
}

func (f *routingFakeStore) GetAllActiveServers() ([]models.Server, error) {
	return f.servers, nil
}

func makeServers(n int) []models.Server {
	out := make([]models.Server, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, models.Server{
			UUID:        string(rune('a'+i)) + "-uuid",
			NodeAddress: "node-token",
		})
	}
	return out
}

// TestRunBatches_PanicIsContainedPerServer covers a bare goroutine that used to
// take the whole Core down.
//
// runBatches fans out one goroutine per server, so a panic inside redeployServer
// happens in a goroutine the caller's recover cannot reach - an unrecovered
// panic there kills the process, not just the migration. A nil queue reproduces
// it: redeployServer dereferences m.queue on its first statement.
//
// Four servers rather than one, because the same loop also updates the shared
// done/failed counters - this exercises the concurrent path that the -race gate
// watches.
func TestRunBatches_PanicIsContainedPerServer(t *testing.T) {
	rdb := newQueueTestRedis(t)
	servers := makeServers(4)
	m := &RoutingMigrationService{
		store: &routingFakeStore{servers: servers},
		queue: nil, // redeployServer panics on this
		redis: rdb,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runBatches(context.Background(), servers, "gateway")
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runBatches did not return; the per-server guard is not releasing the WaitGroup")
	}

	st := m.readStatus(context.Background())
	if st.Running {
		t.Error("status still Running after runBatches returned")
	}
	if st.Failed != len(servers) {
		t.Errorf("Failed = %d, want %d (every server panicked)", st.Failed, len(servers))
	}
	if st.Done != 0 {
		t.Errorf("Done = %d, want 0", st.Done)
	}
}

// TestRunBatches_ConcurrentCounters is the one that actually exercises the data
// race, and it needs the NON-panicking path to run.
//
// The panic test above cannot cover it: with a nil queue every goroutine dies on
// redeployServer's first statement, so the shared done/failed counters are only
// touched from the recover branch. A queue backed by a CLOSED client makes
// SendCommand return an error instead, which is the normal path - four servers
// then increment and read those counters concurrently, which is exactly what
// -race needs to see. Verified: with the counters read outside the mutex (the
// original shape) the detector reports it; as written here it is clean.
func TestRunBatches_ConcurrentCounters(t *testing.T) {
	rdb := newQueueTestRedis(t)

	failing := newQueueTestRedis(t)
	failing.Close() // Publish now errors rather than panicking

	servers := makeServers(4)
	m := &RoutingMigrationService{
		store: &routingFakeStore{servers: servers},
		queue: NewQueueService(failing),
		redis: rdb,
	}

	m.runBatches(context.Background(), servers, "gateway")

	st := m.readStatus(context.Background())
	if st.Failed != len(servers) {
		t.Errorf("Failed = %d, want %d (every send errored)", st.Failed, len(servers))
	}
	if st.Running {
		t.Error("status still Running after runBatches returned")
	}
}

// TestRun_ReleasesTheClusterLock pins the other half: the SETNX lock this job
// takes has a 2h TTL, so stranding it blocks every Core from starting another
// migration for two hours and leaves the panel showing one that is not running.
// The release moved into a defer so it also covers the panic path.
func TestRun_ReleasesTheClusterLock(t *testing.T) {
	rdb := newQueueTestRedis(t)
	servers := makeServers(2)
	m := &RoutingMigrationService{
		store: &routingFakeStore{servers: servers},
		queue: nil,
		redis: rdb,
	}

	n, err := m.Run(context.Background(), "gateway")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != len(servers) {
		t.Fatalf("Run returned %d, want %d", n, len(servers))
	}

	// The work happens in a goroutine; poll rather than sleep a fixed amount.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if rdb.Exists(context.Background(), routingMigrationLockKey).Val() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := rdb.Exists(context.Background(), routingMigrationLockKey).Val(); got != 0 {
		t.Fatal("cluster migration lock was not released; another Core is now blocked for the full TTL")
	}

	if st := m.readStatus(context.Background()); st.Running {
		t.Error("status still Running after the migration goroutine finished")
	}

	// And the lock being gone must actually allow a retry.
	if _, err := m.Run(context.Background(), "gateway"); err != nil {
		t.Errorf("a second Run should be possible once the lock is released, got: %v", err)
	}
	// Wait for that second run too: left running past the test it raced
	// TestComputeNextRun_IsUTCRegardlessOfHostZone's write to time.Local under -race.
	waitForLockRelease(t, rdb)
}

func waitForLockRelease(t *testing.T, rdb *redis.Client) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for rdb.Exists(context.Background(), routingMigrationLockKey).Val() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the migration goroutine did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
