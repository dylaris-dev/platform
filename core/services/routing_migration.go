package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

const (
	routingMigrationKey = "dylaris:routing_migration"
	// routingMigrationLockKey is a Redis SETNX gate preventing
	// two Cores from kicking off the same migration simultaneously. The
	// existing in-process sync.Mutex only covered single-instance Core —
	// multi-instance would otherwise race past the local mutex and both
	// dispatch the batch.
	//
	// The TTL is a backstop, not a bound on the run: batches are 4 servers wide
	// and each server is given up to routingRedeploySettleTimeout, so a large
	// fleet runs far past two hours (10k servers is 2500 batches, i.e. hours,
	// not the "~1h" an earlier version of this comment claimed). A migration
	// that outlives its lock is caught by the Running status instead, which is
	// kept fresh for exactly as long as the run lasts.
	routingMigrationLockKey = "dylaris:routing_migration:lock"
	routingMigrationLockTTL = 2 * time.Hour

	// routingMigrationStatusTTL keeps the FINAL result around for the panel to
	// display after the run ends.
	routingMigrationStatusTTL = 24 * time.Hour
	// routingMigrationRunningTTL bounds how long a Running:true status may
	// outlive the process that wrote it. updateProgress rewrites the status
	// after every server, so a live migration keeps it fresh; a Core that is
	// killed mid-run (the deferred cleanup only covers a panic, not SIGKILL or
	// an OOM) leaves it to expire instead of reporting a migration that no
	// longer exists. That mattered because Run refuses to start while the flag
	// is set, so a crash used to block every routing-mode switch for a day.
	//
	// It has to clear the longest silence between two updates: the last server
	// of a batch can take routingRedeploySettleTimeout, then routingBatchPause
	// passes, then the first server of the next batch can take that long again.
	routingMigrationRunningTTL = 5 * time.Minute

	// routingRedeploySettleTimeout is how long one server is polled for a
	// settled status before it counts as failed. routingBatchPause is the gap between
	// batches. Named so the TTL above can be asserted against them.
	routingRedeploySettleTimeout = 60 * time.Second
	routingBatchPause            = 15 * time.Second
)

// The settle wait as variables, so a test can run it in milliseconds.
var (
	redeploySettleTimeout = routingRedeploySettleTimeout
	redeployPollEvery     = 3 * time.Second
)

type MigrationStatus struct {
	Running bool `json:"running"`
	Total   int  `json:"total"`
	Done    int  `json:"done"`
	Failed  int  `json:"failed"`
}

type RoutingMigrationService struct {
	store store.Store
	queue *QueueService
	redis *redis.Client
	mu    sync.Mutex
}

func NewRoutingMigrationService(s store.Store, q *QueueService, r *redis.Client) *RoutingMigrationService {
	return &RoutingMigrationService{store: s, queue: q, redis: r}
}

// Run kicks off a background batch redeploy of all active servers.
// Returns the number of servers queued.
//
// Concurrency: in addition to the local sync.Mutex (single-process safety),
// a Redis SETNX lock guards against two Cores both kicking off the same
// migration. The lock is released after runBatches finishes. On a leader
// handoff mid-migration the TTL acts as a backstop so the cluster eventually
// recovers without manual intervention.
func (m *RoutingMigrationService) Run(ctx context.Context, newMode string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Cluster-wide lock: SETNX with TTL. Released when the goroutine returns.
	if m.redis != nil {
		acquired, err := m.redis.SetNX(ctx, routingMigrationLockKey, "1", routingMigrationLockTTL).Result()
		if err != nil {
			return 0, fmt.Errorf("acquire migration lock: %w", err)
		}
		if !acquired {
			return 0, fmt.Errorf("migration already in progress on another Core")
		}
	}

	existing := m.readStatus(ctx)
	if existing.Running {
		// Taking the lock means no live run holds it — but a run that outlived
		// the lock's TTL is still going, and the status is the only thing that
		// knows. It expires with the process that writes it
		// (routingMigrationRunningTTL), so a flag that is still here is a live
		// migration, not a leftover from a crash. Give the lock back.
		_ = m.releaseLock(ctx)
		return 0, fmt.Errorf("migration already in progress")
	}

	servers, err := m.store.GetAllActiveServers()
	if err != nil {
		_ = m.releaseLock(ctx)
		return 0, fmt.Errorf("failed to load servers: %w", err)
	}
	if len(servers) == 0 {
		_ = m.releaseLock(ctx)
		return 0, nil
	}

	m.writeStatus(ctx, MigrationStatus{Running: true, Total: len(servers)})
	go func() {
		// Same guard the other two migration jobs use (db_migration_job,
		// storage_migration_job): a panic in a job that holds a cluster-wide
		// lock must not strand it. Without this the SETNX lock sat for its full
		// 2h TTL and the status stayed Running:true, so the panel showed a
		// migration that no longer existed and no Core could start another.
		//
		// The lock release is OUTSIDE the recover check on purpose, so it runs
		// on the normal path too - that is what the deferred form buys over the
		// trailing statement this replaces.
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[RoutingMigration] panicked, releasing the lock and clearing the running flag: %v", rec)
				s := m.readStatus(context.Background())
				s.Running = false
				m.writeStatus(context.Background(), s)
			}
			_ = m.releaseLock(context.Background())
		}()
		m.runBatches(context.Background(), servers, newMode)
	}()
	return len(servers), nil
}

func (m *RoutingMigrationService) releaseLock(ctx context.Context) error {
	if m.redis == nil {
		return nil
	}
	return m.redis.Del(ctx, routingMigrationLockKey).Err()
}

func (m *RoutingMigrationService) runBatches(ctx context.Context, servers []models.Server, newMode string) {
	const batchSize = 4
	done := 0
	failed := 0

	for i := 0; i < len(servers); i += batchSize {
		end := i + batchSize
		if end > len(servers) {
			end = len(servers)
		}
		batch := servers[i:end]

		var wg sync.WaitGroup
		var mu sync.Mutex

		for _, srv := range batch {
			wg.Add(1)
			go func(s models.Server) {
				defer wg.Done()
				// A panic here used to take the whole Core down: this is a bare
				// goroutine, and the recover on the caller cannot reach into it.
				// Count it as a failed server instead, which is what an error
				// from redeployServer would have done anyway.
				defer func() {
					if rec := recover(); rec != nil {
						log.Printf("[RoutingMigration] server %s panicked: %v", s.UUID, rec)
						mu.Lock()
						failed++
						d, f := done, failed
						mu.Unlock()
						m.updateProgress(ctx, d, f, len(servers))
					}
				}()

				err := m.redeployServer(ctx, s, newMode)

				// Read the counters under the SAME lock that increments them.
				// updateProgress used to be called with unsynchronised reads of
				// done/failed while sibling goroutines in the batch were
				// incrementing them - a data race, and one no test reached
				// because it needs two servers migrating at once.
				mu.Lock()
				if err != nil {
					log.Printf("[RoutingMigration] server %s failed: %v", s.UUID, err)
					failed++
				} else {
					done++
				}
				d, f := done, failed
				mu.Unlock()

				m.updateProgress(ctx, d, f, len(servers))
			}(srv)
		}
		wg.Wait()

		if end < len(servers) {
			select {
			case <-ctx.Done():
				goto done
			case <-time.After(routingBatchPause):
			}
		}
	}
done:
	m.writeStatus(ctx, MigrationStatus{Running: false, Total: len(servers), Done: done, Failed: failed})
	log.Printf("[RoutingMigration] complete: %d done, %d failed", done, failed)
}

func (m *RoutingMigrationService) redeployServer(ctx context.Context, srv models.Server, newMode string) error {
	// NodeAddress carries the node token from GetAllActiveServers
	nodeToken := srv.NodeAddress

	// An install or a move recreates the container itself when it ends, and
	// the node binds by the mode current at that moment; a server being
	// stopped is recreated by its next start. Recreating it here raced the
	// install's own last step over the same container name, and rewrote the
	// config of a server whose directory a move was archiving (a move's stop
	// phase reads "stopping"). Read now, not from the fleet load: the batches
	// can reach a server minutes later.
	status := srv.Status
	if latest, err := m.store.GetServerByUUID(srv.UUID); err == nil && latest != nil {
		status = latest.Status
	}
	switch status {
	case "installing", "migrating", "stopping":
		log.Printf("[RoutingMigration] server %s is %s; it takes the new routing when its container is next recreated", srv.UUID, status)
		return nil
	}

	type dockerCfg struct {
		RAM           int     `json:"ram"`
		CPULimit      float64 `json:"cpuLimit"`
		DiskLimit     int64   `json:"diskLimit"`
		Image         string  `json:"image"`
		Command       string  `json:"command"`
		ExtraJvmFlags string  `json:"extraJvmFlags,omitempty"`
		CpusetCpus    string  `json:"cpusetCpus"`
		HostPort      int     `json:"hostPort"`
		ContainerPort int     `json:"containerPort"`
	}
	type serverCfg struct {
		UUID            string    `json:"uuid"`
		ActiveSubServer string    `json:"activeSubServer"`
		Docker          dockerCfg `json:"docker"`
	}

	cfg := serverCfg{
		UUID:            srv.UUID,
		ActiveSubServer: srv.ActiveSubServer,
		Docker: dockerCfg{
			RAM:           srv.Memory,
			CPULimit:      srv.CPULimit,
			DiskLimit:     srv.DiskLimit,
			Image:         srv.GameImage,
			Command:       srv.StartCommand,
			ExtraJvmFlags: srv.ExtraJvmFlags,
			// The effective cpuset (GetAllActiveServers). Without it every
			// recreated container lost its pinning.
			CpusetCpus: srv.Cpuset,
			// hostPort=0 → node auto-allocates (ip_port mode) or skips binding (gateway mode)
			HostPort:      0,
			ContainerPort: srv.ContainerPort,
		},
	}

	if err := m.queue.SendCommand(ctx, nodeToken, "update_resources", cfg, nil); err != nil {
		return err
	}

	// Poll for the container to settle
	deadline := time.Now().Add(redeploySettleTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(redeployPollEvery):
		}
		latest, err := m.store.GetServerByUUID(srv.UUID)
		if err != nil {
			continue
		}
		switch latest.Status {
		// "starting" is the recreated container booting, already bound the
		// new way; a large modpack takes longer than the wait to log "Done".
		case "stopped", "online", "offline", "disk_full", "starting":
			return nil
		}
	}

	// Not settled in time. This used to SIGKILL the server and count it as
	// done: a JVM still shutting down lost the world since its last save, and
	// the kill changed no port binding, so it bought nothing.
	return fmt.Errorf("did not settle within %s", redeploySettleTimeout)
}

func (m *RoutingMigrationService) GetStatus(ctx context.Context) MigrationStatus {
	return m.readStatus(ctx)
}

func (m *RoutingMigrationService) readStatus(ctx context.Context) MigrationStatus {
	val, err := m.redis.Get(ctx, routingMigrationKey).Result()
	if err != nil {
		return MigrationStatus{}
	}
	var s MigrationStatus
	_ = json.Unmarshal([]byte(val), &s)
	return s
}

func (m *RoutingMigrationService) writeStatus(ctx context.Context, s MigrationStatus) {
	data, _ := json.Marshal(s)
	// A running status is a liveness claim and expires with the process that
	// makes it; a finished one is a result and is kept for the panel.
	ttl := routingMigrationStatusTTL
	if s.Running {
		ttl = routingMigrationRunningTTL
	}
	m.redis.Set(ctx, routingMigrationKey, data, ttl)
}

func (m *RoutingMigrationService) updateProgress(ctx context.Context, done, failed, total int) {
	s := m.readStatus(ctx)
	// runBatches is the only caller, so by construction this IS a running
	// migration. Do not inherit Running from the read: readStatus answers with
	// a zero value when the key is missing, and Redis here is in-memory by
	// design, so a restart mid-run would otherwise have this write back
	// "finished" over a migration still in progress - unblocking a second one.
	s.Running = true
	s.Done = done
	s.Failed = failed
	s.Total = total
	m.writeStatus(ctx, s)
}
