package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"dylaris-core/database"
	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/services/redisacl"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"

	"dylaris-pkg/migration"
	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"
)

// MigrationOrchestrator drives node-to-node server migrations (auto-move). It is
// leader-gated: only the elected Core consumes the request queue and runs the
// step machine, so manual moves and (Wave 4) rebalance moves share one executor
// and survive the requesting Core restarting.
//
// v1 limitations (acceptable, noted deliberately):
//   - Migrations are processed SEQUENTIALLY, one at a time. Safe and simple;
//     throughput is not a concern for an admin-triggered / rebalance flow.
//   - If the leader dies mid-migration the queue redelivers the request to the
//     next leader, which waits out the dead leader's per-server lock and re-runs
//     the move (holdMigrationLock). We never delete source data before cutover,
//     so no data loss.
type MigrationOrchestrator struct {
	store         store.Store
	redis         *redis.Client
	queue         *QueueService
	gateway       GatewayProvider
	leader        leader.Election
	clusterSecret string
	// cpuPinning re-resolves a server's CPU pinning for the TARGET node at
	// cutover (auto recomputed, manual reset when the hardware differs). nil-safe.
	cpuPinning *CPUPinningService
}

// SetCPUPinning wires the CPU-pinning service. Call once at boot (optional).
func (o *MigrationOrchestrator) SetCPUPinning(c *CPUPinningService) { o.cpuPinning = c }

// migrationStreamKey is the durable Redis Stream the manual endpoint (and the
// rebalance worker) publish requests onto; the elected leader consumes them via
// a consumer group. New suffix avoids a WRONGTYPE collision with the old
// `dylaris:migration:queue` list.
const migrationStreamKey = "dylaris:migration:stream"

// migrationGroup / migrationConsumer name the single logical consumer. The
// consumer name is fixed (not per-instance) so when leadership moves to another
// Core, that Core's consumer recovers the previous leader's pending entries.
const (
	migrationGroup    = "migration"
	migrationConsumer = "migration-worker"
)

// MigrationRequest is the JSON payload on the migration queue.
type MigrationRequest struct {
	ServerID     int    `json:"serverID"`
	TargetNodeID int    `json:"targetNodeID"`
	Reason       string `json:"reason"`      // "manual" | "rebalance"
	RequestedBy  string `json:"requestedBy"` // user ID or "system"
}

// orchestrationStatus is the orchestrator-owned progress record, written to
// dylaris:migration:<uuid>:orchestration so the panel (Wave 5) can poll. It is
// distinct from the node-owned :status / :meta keys — we never clobber those.
type orchestrationStatus struct {
	Phase        string `json:"phase"`
	Error        string `json:"error,omitempty"`
	SourceNodeID int    `json:"sourceNodeID"`
	TargetNodeID int    `json:"targetNodeID"`
	Reason       string `json:"reason"`
	StartedAt    int64  `json:"startedAt"` // unix seconds
	UpdatedAt    int64  `json:"updatedAt"` // unix seconds
}

const (
	// migrationLockTTL is how long the per-server migration lock survives
	// WITHOUT a refresh - that is, how long a crashed Core's lock lingers. It is
	// NOT a bound on how long a migration may run: holdMigrationLock refreshes
	// it at a third of this for as long as the work lasts. It has to be short,
	// because a stale lock is waited out synchronously (migrationLockWait).
	migrationLockTTL = 60 * time.Second
	// migrationLockWait is how long one delivery waits for a held lock before
	// giving up. It MUST exceed migrationLockTTL so a dead Core's leftover lock
	// is always outlived; a lock still held after this belongs to a migration
	// that is genuinely alive elsewhere.
	migrationLockWait        = 90 * time.Second
	migrationLockPoll        = 1 * time.Second
	orchestrationStatusTTL   = time.Hour
	migrationStopTimeout     = 90 * time.Second
	migrationStageTimeout    = 5 * time.Minute  // archiving GBs on the source
	migrationTransferTimeout = 30 * time.Minute // multi-GB transfer to target
	// migrationR2PhaseTimeout bounds each leg (upload, then download) of the
	// cross-LAN BYON R2 fallback. Longer than the LAN/overlay transfer because a
	// BYON home uplink is slow; still capped so one stuck transfer can't block the
	// serialized migration queue indefinitely. Within the BYON presign TTL (6h).
	migrationR2PhaseTimeout    = 1 * time.Hour
	migrationStartTimeout      = 90 * time.Second // best-effort online confirm
	migrationPollInterval      = 2 * time.Second
	migrationTokenTTL          = 15 * time.Minute
	migrationQueueBlockTimeout = 5 * time.Second // BLPOP timeout; not a hot spin
)

func NewMigrationOrchestrator(s store.Store, r *redis.Client, q *QueueService, g GatewayProvider, clusterSecret string) *MigrationOrchestrator {
	return &MigrationOrchestrator{
		store:         s,
		redis:         r,
		queue:         q,
		gateway:       g,
		clusterSecret: clusterSecret,
	}
}

// SetLeader wires the leader-election gate. The queue consumer only runs on the
// elected Core; followers idle.
func (o *MigrationOrchestrator) SetLeader(l leader.Election) { o.leader = l }

// Start launches the queue-consumer goroutine. Returns immediately.
func (o *MigrationOrchestrator) Start(ctx context.Context) {
	log.Println("Migration Orchestrator started")
	go o.consume(ctx)
}

// EnqueueMigration pushes a migration request onto the queue. Callable from any
// Core (the manual endpoint, the Wave 4 worker); the leader executes it.
func (o *MigrationOrchestrator) EnqueueMigration(ctx context.Context, serverID, targetNodeID int, reason, requestedBy string) error {
	req := MigrationRequest{
		ServerID:     serverID,
		TargetNodeID: targetNodeID,
		Reason:       reason,
		RequestedBy:  requestedBy,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal migration request: %w", err)
	}
	if _, err := queue.Publish(ctx, o.redis, migrationStreamKey, data); err != nil {
		return fmt.Errorf("enqueue migration request: %w", err)
	}
	return nil
}

// consume is the leader-gated durable-queue loop. When not leader it idles on a
// short timer so a follower that becomes leader picks up pending requests
// quickly without hot-spinning. When leader it runs a consumer-group reader
// (Concurrency 1 = one migration at a time) until leadership is lost.
//
// Durability: a request is ACKed only after Migrate returns, so a leader crash
// mid-migration leaves the request pending; the next leader recovers it (same
// fixed consumer name) and re-runs Migrate. Source data is never deleted before
// cutover, so there is no data-loss window.
//
// That recovery runs straight into the dead leader's leftover per-server lock,
// and this handler ACKs whatever Migrate does — so Migrate must not treat a held
// lock as a reason to return. holdMigrationLock waits it out instead; see the
// "TOO LONG" half of its comment.
func (o *MigrationOrchestrator) consume(ctx context.Context) {
	consumer := queue.NewConsumer(o.redis, migrationStreamKey, migrationGroup, migrationConsumer)
	consumer.Concurrency = 1 // migrations are serialized

	handler := func(hctx context.Context, data []byte) error {
		var req MigrationRequest
		if err := json.Unmarshal(data, &req); err != nil {
			log.Printf("migration: bad request payload, dropping: %v", err)
			return nil // ack + drop malformed
		}
		o.Migrate(hctx, req)
		return nil // ack: Migrate records its own status/rollback
	}

	backoff := time.Duration(0)
	for {
		if ctx.Err() != nil {
			return
		}
		// Only the elected leader consumes; followers idle.
		if o.leader != nil && !o.leader.IsLeader() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(migrationQueueBlockTimeout):
			}
			continue
		}
		// Run until this Core loses leadership (leaderCtx cancelled) or shutdown.
		leaderCtx, cancel := context.WithCancel(ctx)
		go o.watchLeadership(leaderCtx, cancel)
		err := consumer.Run(leaderCtx, handler)
		cancel()

		// Run never returns nil: it returns either a setup failure or the
		// context error it stopped on. A cancellation is this loop working as
		// designed - shutdown, or leadership handed over - so it resets the
		// backoff and goes straight back round, where the leader check parks it.
		if isContextError(err) {
			backoff = 0
			continue
		}

		// Anything else means Run could not get as far as its own read loop,
		// which in practice means EnsureGroup could not reach Redis. That
		// returns immediately, and without a delay here the loop simply went
		// straight back round, spawning a watchLeadership goroutine per turn
		// and never widening the gap however long the outage lasted.
		//
		// Worth being accurate about the severity: this was not a CPU-burning
		// spin. A refused dial costs about 2.1s against the configured client,
		// because go-redis retries it internally before returning (measured,
		// not assumed), so the loop turned every couple of seconds. The reason
		// to fix it is the unbounded retry rate and the goroutine per turn,
		// not a pegged core.
		backoff = nextMigrationBackoff(backoff)
		// Say plainly when waiting cannot help. A rejected credential or a
		// missing ACL grant produces the same steady retry line as a Redis
		// restart, and an operator watching the log has no way to tell that one
		// of them is waiting for them.
		if failure := database.ClassifyRedisError(err); failure.NeedsOperator() {
			log.Printf("migration: queue consumer cannot reach the queue and this will NOT clear on its own (%s): %v - retrying in %s", failure.Slug(), err, backoff)
		} else {
			log.Printf("migration: queue consumer stopped (%v), retrying in %s", err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// Migration consumer retry bounds. These are no longer tied to the per-server
// lock TTL: recovering an interrupted migration used to depend on the queue
// resuming AFTER the stale lock had expired, which is a race the backoff cannot
// win and, lost, dropped the request. holdMigrationLock waits the lock out
// instead, so the recovery is correct whenever the queue comes back. What is
// left here is only how fast a consumer retries a Redis it cannot reach.
const (
	migrationRetryInitial = 1 * time.Second
	migrationRetryMax     = 30 * time.Second
)

// nextMigrationBackoff doubles up to the ceiling, starting at the initial delay.
func nextMigrationBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return migrationRetryInitial
	}
	next := current * 2
	if next > migrationRetryMax {
		return migrationRetryMax
	}
	return next
}

// isContextError reports whether err is a context cancellation or deadline,
// including one wrapped by a caller.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// watchLeadership cancels leaderCtx as soon as this Core stops being the leader,
// so the migration consumer stops reading (only the leader migrates). A nil
// leader (single-Core dev mode) means run unconditionally.
func (o *MigrationOrchestrator) watchLeadership(ctx context.Context, cancel context.CancelFunc) {
	if o.leader == nil {
		return
	}
	t := time.NewTicker(migrationQueueBlockTimeout)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !o.leader.IsLeader() {
				cancel()
				return
			}
		}
	}
}

// Migrate runs the full step machine for one request. Errors are logged and
// surfaced via the orchestration status key; we never return them to a caller
// (this runs async on the leader).
func (o *MigrationOrchestrator) Migrate(ctx context.Context, req MigrationRequest) {
	// --- (a) Load + validate ---
	srv, err := o.store.GetServerByID(req.ServerID)
	if err != nil {
		log.Printf("migration: server %d not found: %v", req.ServerID, err)
		return
	}
	sourceNode, err := o.store.GetNodeByID(srv.NodeID)
	if err != nil {
		log.Printf("migration %s: source node %d not found: %v", srv.UUID, srv.NodeID, err)
		o.writeStatus(ctx, srv.UUID, "failed", "source node not found", srv.NodeID, req.TargetNodeID, req.Reason, time.Now())
		return
	}
	targetNode, err := o.store.GetNodeByID(req.TargetNodeID)
	if err != nil {
		log.Printf("migration %s: target node %d not found: %v", srv.UUID, req.TargetNodeID, err)
		o.writeStatus(ctx, srv.UUID, "failed", "target node not found", srv.NodeID, req.TargetNodeID, req.Reason, time.Now())
		return
	}

	startedAt := time.Now()
	writeStatus := func(phase, errMsg string) {
		o.writeStatus(ctx, srv.UUID, phase, errMsg, sourceNode.ID, targetNode.ID, req.Reason, startedAt)
	}

	// Migration is gateway-only: a server's reachable address is tied to its
	// node unless the gateway re-points the route, so a move without it breaks
	// the server.
	if !o.gatewayEnabled() {
		log.Printf("migration %s: gateway disabled, refusing", srv.UUID)
		writeStatus("failed", "gateway routing disabled")
		return
	}
	if targetNode.ID == sourceNode.ID {
		log.Printf("migration %s: target == source (%d), refusing", srv.UUID, targetNode.ID)
		writeStatus("failed", "target node equals source node")
		return
	}
	if targetNode.Status != "online" {
		log.Printf("migration %s: target node %d not online (%s)", srv.UUID, targetNode.ID, targetNode.Status)
		writeStatus("failed", "target node not online")
		return
	}

	// --- (b) Acquire per-server lock ---
	releaseLock, err := o.holdMigrationLock(ctx, srv.UUID, req.RequestedBy,
		migrationLockTTL, migrationLockWait, migrationLockPoll)
	if errors.Is(err, errMigrationLockHeld) {
		// Still held after the full wait, so it is not a dead Core's leftover:
		// another Core owns this migration and will finish it.
		log.Printf("migration %s: still locked after %s, another Core is migrating it - skipping", srv.UUID, migrationLockWait)
		return
	}
	if isContextError(err) {
		// Leadership handover or shutdown while waiting out the lock. This is
		// NOT a failure of this migration: the handler's ACK runs on the same
		// cancelled context and fails too, so the request stays pending and the
		// next leader re-runs it. Returning through the branch below would
		// depend on writeStatus quietly no-opping for the same reason - true
		// today, and not something to leave resting on.
		log.Printf("migration %s: stopped while waiting for the lock (%v) - left pending for the next leader", srv.UUID, err)
		return
	}
	if err != nil {
		log.Printf("migration %s: lock error: %v", srv.UUID, err)
		writeStatus("failed", "lock error")
		return
	}
	defer releaseLock()
	// Always clear any admin cancel flag when the migration ends, so a too-late
	// cancel (arrived post-cutover and ignored) never lingers into a future move.
	defer o.redis.Del(context.Background(), migrationCancelKey(srv.UUID))

	// --- (c) Starting ---
	writeStatus("starting", "")
	wasRunning := srv.Status == "online"
	preStatus := srv.Status

	// --- (d) Rebalance player gate (manual moves skip — admin chose to move) ---
	if req.Reason == "rebalance" && wasRunning {
		if players := o.currentPlayerCount(ctx, srv.UUID); players > 0 {
			log.Printf("migration %s: aborting rebalance, %d players online", srv.UUID, players)
			writeStatus("aborted_players", fmt.Sprintf("%d players online", players))
			return
		}
	}

	// --- (e) Stop the running server ---
	if wasRunning {
		writeStatus("migrating", "")
		if err := o.stopServer(ctx, srv, sourceNode); err != nil {
			log.Printf("migration %s: stop failed: %v", srv.UUID, err)
			o.rollbackPreCutover(ctx, srv, sourceNode, wasRunning, preStatus, writeStatus, "stop failed")
			return
		}
		if !o.waitForStopped(ctx, srv.ID, migrationStopTimeout) {
			log.Printf("migration %s: stop timed out", srv.UUID)
			o.rollbackPreCutover(ctx, srv, sourceNode, wasRunning, preStatus, writeStatus, "stop timed out")
			return
		}
	}

	// --- (f) migrate_out (source stages archive) ---
	o.store.UpdateServerStatus(srv.ID, "migrating")
	writeStatus("migrating", "")
	if err := o.queue.SendMigrateOutCommand(ctx, sourceNode.Token, srv.UUID); err != nil {
		log.Printf("migration %s: migrate_out queue failed: %v", srv.UUID, err)
		o.rollbackPreCutover(ctx, srv, sourceNode, wasRunning, preStatus, writeStatus, "migrate_out queue failed")
		return
	}
	if phase, nerr := o.waitForNodePhase(ctx, sourceNode.Token, srv.UUID, "staged", migrationStageTimeout); phase != "staged" {
		log.Printf("migration %s: staging failed (phase=%s, err=%s)", srv.UUID, phase, nerr)
		o.rollbackPreCutover(ctx, srv, sourceNode, wasRunning, preStatus, writeStatus, "staging failed: "+nerr)
		return
	}

	// --- (g) migrate_in (target pulls + verifies) ---
	meta, err := o.readMeta(ctx, sourceNode.Token, srv.UUID)
	if err != nil {
		log.Printf("migration %s: cannot read meta: %v", srv.UUID, err)
		o.rollbackPreCutover(ctx, srv, sourceNode, wasRunning, preStatus, writeStatus, "meta read failed")
		return
	}
	// Key the pull token with the SOURCE node's per-node secret so a stolen token is
	// scoped to that one node, not the whole cluster.
	secret, ok, serr := redisacl.LoadNodeSecret(o.store, o.clusterSecret, sourceNode.ID)
	if serr != nil || !ok {
		log.Printf("migration %s: source node secret unavailable: %v", srv.UUID, serr)
		o.rollbackPreCutover(ctx, srv, sourceNode, wasRunning, preStatus, writeStatus, "source node secret unavailable")
		return
	}
	migKey := string(secret)
	token, err := migration.MintToken(migKey, srv.UUID, strconv.Itoa(sourceNode.ID), migrationTokenTTL)
	if err != nil {
		log.Printf("migration %s: mint token failed: %v", srv.UUID, err)
		o.rollbackPreCutover(ctx, srv, sourceNode, wasRunning, preStatus, writeStatus, "token mint failed")
		return
	}
	// sourceNodeID is the node ID as a string — the target resolves the pull
	// endpoint via dylaris:migration:endpoint:<sourceNodeID>.
	// See migrationSourceLANIPs for who is handed the source's LAN addresses.
	sourcePrivateIPs := migrationSourceLANIPs(sourceNode, targetNode)
	if err := o.queue.SendMigrateInCommand(ctx, targetNode.Token, srv.UUID, strconv.Itoa(sourceNode.ID), token, meta.SHA256, meta.Size, sourcePrivateIPs); err != nil {
		log.Printf("migration %s: migrate_in queue failed: %v", srv.UUID, err)
		o.rollbackPreCutover(ctx, srv, sourceNode, wasRunning, preStatus, writeStatus, "migrate_in queue failed")
		return
	}
	// migrate_in reports "transferred" on success, or "need_remote" when this is
	// a BYON move and the target cannot reach the source over the LAN. In the
	// latter case we transfer through R2 (node-direct, no warp hairpin) instead.
	phase, nerr := o.waitForNodePhaseAny(ctx, targetNode.Token, srv.UUID, map[string]bool{"transferred": true, "need_remote": true}, migrationTransferTimeout)
	if phase == "need_remote" {
		log.Printf("migration %s: source LAN unreachable, falling back to R2 transfer", srv.UUID)
		if err := o.transferViaR2(ctx, srv, sourceNode, targetNode, meta.SHA256, meta.Size); err != nil {
			// Still pre-cutover: node_id unchanged, source authoritative — roll back.
			log.Printf("migration %s: R2 transfer failed: %v", srv.UUID, err)
			o.rollbackPreCutover(ctx, srv, sourceNode, wasRunning, preStatus, writeStatus, "r2 transfer failed: "+err.Error())
			return
		}
		// R2 transfer verified the archive on the target — fall through to cutover.
	} else if phase != "transferred" {
		// Target self-cleans its partial (Wave 2b). node_id is NOT yet changed,
		// so the source remains authoritative — safe to roll back.
		log.Printf("migration %s: transfer failed (phase=%s, err=%s)", srv.UUID, phase, nerr)
		o.rollbackPreCutover(ctx, srv, sourceNode, wasRunning, preStatus, writeStatus, "transfer failed: "+nerr)
		return
	}

	// Final pre-cutover cancel check: if an admin requested cancellation while the
	// last transfer wait was completing, roll back now (still pre-cutover, the
	// source is authoritative) instead of cutting over.
	if o.cancelRequested(ctx, srv.UUID) {
		log.Printf("migration %s: cancelled by admin before cutover", srv.UUID)
		o.rollbackPreCutover(ctx, srv, sourceNode, wasRunning, preStatus, writeStatus, "cancelled before cutover")
		return
	}

	// --- (h) CUTOVER: flip node_id + re-point route. Target now authoritative. ---
	if err := o.store.UpdateServerNode(srv.ID, targetNode.ID); err != nil {
		log.Printf("migration %s: UpdateServerNode failed: %v", srv.UUID, err)
		// Pre-cutover from the data POV (node_id flip failed, source still owns
		// data), so roll back like the earlier steps.
		o.rollbackPreCutover(ctx, srv, sourceNode, wasRunning, preStatus, writeStatus, "node reassign failed")
		return
	}
	if err := o.gateway.MigrateServerRoutes(uint(srv.ID), uint(targetNode.ID)); err != nil {
		// node_id already flipped; data is on the target. Route re-point failed,
		// but reverting node_id would point the route back at data the source is
		// about to be told to keep — and we have not sent cleanup yet. Surface
		// the failure; the route can be re-pointed manually / on retry. Do not
		// revert node_id (target is authoritative now).
		log.Printf("migration %s: MigrateServerRoutes failed post node-flip: %v", srv.UUID, err)
		writeStatus("failed_post_cutover", "route re-point failed: "+err.Error())
		return
	}

	// Re-resolve CPU pinning for the target host. Different hardware (fewer cores,
	// different P/E or X3D layout) means the source cpuset is no longer valid or
	// meaningful: auto is recomputed on the target, manual is reset to shared
	// unless the target is identical hardware. Persisted here; a running pinned
	// server is recreated with the corrected cpuset below instead of a plain start.
	effCpuset, pinned := o.reResolvePinningForTarget(ctx, srv, sourceNode, targetNode)

	// --- (i) Start on target if it was running (best-effort) ---
	if wasRunning {
		o.store.UpdateServerStatus(srv.ID, "starting")
		o.store.UpdateServerDesiredState(srv.ID, "online")
		// Orchestration phase "finalizing" (not "starting") marks POST-cutover: it
		// disambiguates from the pre-cutover "starting" phase so the cancel endpoint
		// only offers cancellation while still pre-cutover.
		writeStatus("finalizing", "")
		var startErr error
		if pinned {
			// Recreate with the corrected cpuset (and current resources) so the
			// container lands on valid target cores, then it comes up online.
			startErr = o.queue.SendCommand(ctx, targetNode.Token, "update_resources", map[string]interface{}{
				"uuid":            srv.UUID,
				"activeSubServer": srv.ActiveSubServer,
				"docker": map[string]interface{}{
					"ram":        srv.Memory,
					"cpuLimit":   srv.CPULimit,
					"diskLimit":  srv.DiskLimit,
					"cpusetCpus": effCpuset,
					"image":      srv.GameImage,
					"command":    srv.StartCommand,
				},
			}, nil)
		} else {
			startErr = o.queue.SendCommand(ctx, targetNode.Token, "start", map[string]interface{}{"uuid": srv.UUID}, nil)
		}
		if startErr != nil {
			// Data is safe on the target; only the auto-start dispatch failed.
			log.Printf("migration %s: start on target queue failed: %v", srv.UUID, startErr)
			writeStatus("failed_post_cutover", "start on target failed: "+startErr.Error())
			// Still send cleanup — the move itself succeeded.
		} else {
			// Best-effort confirm; do not fail the migration if start is slow.
			o.waitForOnline(ctx, srv.ID, migrationStartTimeout)
		}
	} else {
		o.store.UpdateServerStatus(srv.ID, "stopped")
		o.store.UpdateServerDesiredState(srv.ID, "stopped")
	}

	// --- (j) Cleanup source copy ---
	if err := o.queue.SendMigrateCleanupCommand(ctx, sourceNode.Token, srv.UUID); err != nil {
		// Non-fatal: the move succeeded, this just leaves a stale copy on the
		// source that an operator / orphan sweep can reclaim.
		log.Printf("migration %s: cleanup queue failed (stale copy left on source %d): %v", srv.UUID, sourceNode.ID, err)
	}

	// --- (k) Final status ---
	// The server's status was already set in step (i): "starting" if it was
	// running (StatusWatcher will flip it to "online") or "stopped" otherwise.
	writeStatus("done", "")
	log.Printf("migration %s: done (node %d -> %d, reason=%s)", srv.UUID, sourceNode.ID, targetNode.ID, req.Reason)
}

// errMigrationLockHeld means another Core holds this server's migration lock and
// kept holding it for the whole wait, so the work is genuinely alive elsewhere.
// Distinct from a lock ERROR, which is this Core failing to talk to Redis.
var errMigrationLockHeld = errors.New("migration lock held by another core")

// holdMigrationLock takes the per-server migration lock and keeps it alive for
// as long as the migration runs. Returns a release func, or an error.
//
// Four places read this key as "this server is migrating right now": the cancel
// endpoint ("in progress iff the lock is held"), the migration-status endpoint's
// cancellable flag, the rebalance worker's skip check, and this function. A
// one-shot SETNX with a fixed TTL cannot make that true, and got it wrong in
// both directions:
//
//   - TOO SHORT. The orchestrator's own budgets below allow 90s stop + 5m
//     staging + 30m transfer on the LAN path, and an hour per leg on the BYON R2
//     path. The lock lapsed under a migration that was still running, after
//     which Cancel answered "No migration is in progress" with a 409 and the
//     panel hid the button - for exactly the long migrations someone would want
//     to cancel - and the rebalance worker stopped skipping the server. So the
//     lock is refreshed while the work lasts.
//
//   - TOO LONG. A Core that dies mid-migration leaves the lock behind for the
//     rest of its TTL, and the queue redelivers the request as soon as the
//     process is back (Consumer.Run recovers its own pending entries before
//     taking new work) - well inside that window. Bailing there was not a pause
//     but a drop: the handler returns nil, so the entry is ACKed and
//     dedup-marked and nothing ever retries it, leaving the server stopped with
//     desired_state=stopped so the node reconciler will not bring it back. So a
//     held lock is waited out, and only a lock that survives the whole wait is
//     treated as somebody else's live work.
//
// ttl, wait and poll are parameters only so the tests can drive the loop; the
// single caller passes the migrationLock* constants.
func (o *MigrationOrchestrator) holdMigrationLock(ctx context.Context, uuid, requestedBy string, ttl, wait, poll time.Duration) (func(), error) {
	lockKey := fmt.Sprintf("dylaris:server:%s:migration", uuid)
	deadline := time.Now().Add(wait)
	for {
		acquired, err := o.redis.SetNX(ctx, lockKey, requestedBy, ttl).Result()
		if err != nil {
			return nil, fmt.Errorf("migration lock SETNX: %w", err)
		}
		if acquired {
			break
		}
		if !time.Now().Before(deadline) {
			return nil, errMigrationLockHeld
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				// SET, not EXPIRE: if the key did lapse - a Redis outage longer
				// than the TTL - EXPIRE on a missing key is a no-op and the lock
				// would stay gone for the rest of the migration.
				o.redis.Set(context.Background(), lockKey, requestedBy, ttl)
			}
		}
	}()
	return func() {
		close(done)
		// Wait for the refresher to be gone before deleting, or a tick already
		// in flight puts the lock straight back after the migration ended.
		<-stopped
		o.redis.Del(context.Background(), lockKey)
	}, nil
}

// rollbackPreCutover restores the server's DB status to its pre-migration value
// and, if it was running and we stopped it, re-starts it on the SOURCE node.
// Never deletes source data — by definition this is called before cutover, so
// the source is still authoritative. Writes orchestration phase "failed".
func (o *MigrationOrchestrator) rollbackPreCutover(ctx context.Context, srv *models.Server, sourceNode *models.Node, wasRunning bool, preStatus string, writeStatus func(string, string), reason string) {
	if wasRunning {
		o.store.UpdateServerDesiredState(srv.ID, "online")
		o.store.UpdateServerStatus(srv.ID, "starting")
		if err := o.queue.SendCommand(ctx, sourceNode.Token, "start", map[string]interface{}{"uuid": srv.UUID}, nil); err != nil {
			log.Printf("migration %s: rollback restart on source failed: %v", srv.UUID, err)
		}
	} else {
		o.store.UpdateServerStatus(srv.ID, preStatus)
	}
	// If this rollback was triggered by an admin cancel, record it as the terminal
	// "cancelled" phase the panel recognises rather than a generic failure.
	if o.cancelRequested(ctx, srv.UUID) {
		writeStatus("cancelled", "migration cancelled by admin")
		return
	}
	writeStatus("failed", reason)
}

// --- Helpers ---

func (o *MigrationOrchestrator) gatewayEnabled() bool {
	mode, _ := o.store.GetSetting("routing_mode")
	if mode == "" {
		mode = "ip_port"
	}
	return mode == "gateway" || mode == "both"
}

// migrationCancelKey is the admin cancel flag for an in-flight migration. The
// cancel endpoint SETs it; the pre-cutover poll loops and the pre-cutover guard
// check it and roll the migration back to the (still authoritative) source. It
// has no effect once node_id has flipped at cutover.
func migrationCancelKey(serverUUID string) string {
	return fmt.Sprintf("dylaris:server:%s:migration:cancel", serverUUID)
}

// cancelRequested reports whether an admin has requested cancellation of this
// server's in-flight migration. Best-effort: a Redis error reads as "not
// cancelled" so a transient blip (or a cancelled ctx on leadership loss) never
// aborts a healthy migration by itself.
func (o *MigrationOrchestrator) cancelRequested(ctx context.Context, serverUUID string) bool {
	n, err := o.redis.Exists(ctx, migrationCancelKey(serverUUID)).Result()
	return err == nil && n > 0
}

// stopServer sends a stop and sets desired_state=stopped so the node reconciler
// doesn't fight the stop by restarting the container.
func (o *MigrationOrchestrator) stopServer(ctx context.Context, srv *models.Server, node *models.Node) error {
	o.store.UpdateServerDesiredState(srv.ID, "stopped")
	o.store.UpdateServerStatus(srv.ID, "stopping")
	return o.queue.SendCommand(ctx, node.Token, "stop", map[string]interface{}{"uuid": srv.UUID}, nil)
}

// waitForStopped polls the DB status (synced from Redis by StatusWatcher) until
// the server reports stopped/offline or the timeout elapses.
func (o *MigrationOrchestrator) waitForStopped(ctx context.Context, serverID int, timeout time.Duration) bool {
	return o.pollDBStatus(ctx, serverID, timeout, func(status string) bool {
		return status == "stopped" || status == "offline"
	})
}

// waitForOnline polls the DB status until "online" or the timeout. Best-effort.
func (o *MigrationOrchestrator) waitForOnline(ctx context.Context, serverID int, timeout time.Duration) bool {
	return o.pollDBStatus(ctx, serverID, timeout, func(status string) bool {
		return status == "online"
	})
}

func (o *MigrationOrchestrator) pollDBStatus(ctx context.Context, serverID int, timeout time.Duration, done func(string) bool) bool {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(migrationPollInterval)
	defer ticker.Stop()
	for {
		srv, err := o.store.GetServerByID(serverID)
		if err == nil && done(srv.Status) {
			return true
		}
		// Admin cancel: return "not done" so the caller rolls back (pre-cutover
		// callers) or ends its best-effort wait (post-cutover waitForOnline).
		if err == nil && o.cancelRequested(ctx, srv.UUID) {
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// waitForNodePhase polls the status key of the node it is WAITING ON until that
// node reaches wantPhase or "error", or the timeout elapses. Returns the last
// observed phase + any error message. A timeout returns the last phase seen
// (often "" if the node never wrote one).
//
// The key is named by nodeToken, so a phase can only come from the node that
// was asked for it. It used to be dylaris:migration:<uuid>:status, which every
// node in the fleet could write - and "transferred" here is what makes the
// caller flip node_id and delete the source copy. See queue.MigrationStatusKey.
func (o *MigrationOrchestrator) waitForNodePhase(ctx context.Context, nodeToken, serverUUID, wantPhase string, timeout time.Duration) (string, string) {
	key := queue.MigrationStatusKey(nodeToken, serverUUID)
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(migrationPollInterval)
	defer ticker.Stop()

	lastPhase := ""
	for {
		raw, err := o.redis.Get(ctx, key).Result()
		if err == nil && raw != "" {
			var st struct {
				Phase string `json:"phase"`
				Error string `json:"error,omitempty"`
			}
			if json.Unmarshal([]byte(raw), &st) == nil {
				lastPhase = st.Phase
				if st.Phase == wantPhase {
					return st.Phase, ""
				}
				if st.Phase == "error" {
					return st.Phase, st.Error
				}
			}
		}
		if o.cancelRequested(ctx, serverUUID) {
			return lastPhase, "cancelled by admin"
		}
		if time.Now().After(deadline) {
			return lastPhase, o.timeoutReason(ctx, lastPhase, serverUUID)
		}
		select {
		case <-ctx.Done():
			return lastPhase, "context cancelled"
		case <-ticker.C:
		}
	}
}

// timeoutReason names the ONE cause an operator cannot guess: a node old enough
// to still report progress under the fleet-wide key nobody reads any more.
//
// The migration keys moved under the reporting node's own token so that Redis
// can refuse a forged phase (see queue.MigrationStatusKey). A node built before
// that writes the old name, this poll never sees it, and the move fails with a
// bare "timed out" that says nothing about what to do. The legacy key is used
// ONLY to tell those two apart - anything on the platform could have written it,
// so it never decides anything.
func (o *MigrationOrchestrator) timeoutReason(ctx context.Context, lastPhase, serverUUID string) string {
	if lastPhase != "" {
		return "timed out"
	}
	if n, err := o.redis.Exists(ctx, queue.LegacyMigrationStatusKey(serverUUID)).Result(); err == nil && n > 0 {
		return "timed out: a node in this move is too old to report progress securely - update it and try again"
	}
	return "timed out"
}

// waitForNodePhaseAny is waitForNodePhase for a SET of acceptable terminal
// phases. It returns as soon as the node reports any phase in `accept` (or
// "error", with its message), or the timeout elapses. Used by migrate_in, which
// can end in either "transferred" (got the copy) or "need_remote" (BYON LAN
// unreachable, use the R2 fallback).
func (o *MigrationOrchestrator) waitForNodePhaseAny(ctx context.Context, nodeToken, serverUUID string, accept map[string]bool, timeout time.Duration) (string, string) {
	key := queue.MigrationStatusKey(nodeToken, serverUUID)
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(migrationPollInterval)
	defer ticker.Stop()

	lastPhase := ""
	for {
		raw, err := o.redis.Get(ctx, key).Result()
		if err == nil && raw != "" {
			var st struct {
				Phase string `json:"phase"`
				Error string `json:"error,omitempty"`
			}
			if json.Unmarshal([]byte(raw), &st) == nil {
				lastPhase = st.Phase
				if accept[st.Phase] {
					return st.Phase, ""
				}
				if st.Phase == "error" {
					return st.Phase, st.Error
				}
			}
		}
		if o.cancelRequested(ctx, serverUUID) {
			return lastPhase, "cancelled by admin"
		}
		if time.Now().After(deadline) {
			return lastPhase, o.timeoutReason(ctx, lastPhase, serverUUID)
		}
		select {
		case <-ctx.Done():
			return lastPhase, "context cancelled"
		case <-ticker.C:
		}
	}
}

// transferViaR2 is the cross-LAN BYON fallback transport. The source uploads its
// already-staged archive to a temporary R2 object (pre-signed PUT) and the target
// downloads it (pre-signed GET), verifies the sha256, and extracts — node-direct,
// $0 egress, no warp hairpin, and the two nodes never need to be reachable to
// each other. The transfer object is temporary (NOT a backup_run, so it never
// counts against the tenant's R2 quota) and is deleted when we're done, success
// or fail. On success the target has reported "transferred" exactly like
// migrate_in, so the caller proceeds to the normal cutover.
func (o *MigrationOrchestrator) transferViaR2(ctx context.Context, srv *models.Server, sourceNode, targetNode *models.Node, expectedSha256 string, expectedSize int64) error {
	bs, err := o.store.GetDefaultBackupStorage()
	if err != nil {
		return fmt.Errorf("resolve backup storage: %w", err)
	}
	if bs == nil || bs.Provider != "s3" {
		return fmt.Errorf("cross-LAN BYON transfer requires an S3/R2 backup storage (none configured)")
	}
	prov, err := backupstorage.Open(ctx, bs, backupstorage.Deps{})
	if err != nil {
		return fmt.Errorf("open backup storage: %w", err)
	}

	// One stable key per server (migrations are serialized, so no collision).
	key := fmt.Sprintf("migration-transfer/%s.zip", srv.UUID)
	// Use the BYON presign TTL (longer) since at least one leg is a slow home link.
	ttl := presignTTL(o.store, true)
	putURL, err := prov.UploadURL(ctx, key, ttl)
	if err != nil || putURL == "" {
		return fmt.Errorf("presign put url: %v", err)
	}
	getURL, err := prov.DownloadURL(ctx, key, ttl)
	if err != nil || getURL == "" {
		return fmt.Errorf("presign get url: %v", err)
	}
	// Always remove the temporary transfer object — on success it's redundant once
	// the target has it, on failure we must not leak it. Background ctx so a
	// cancelled migration still cleans up.
	defer func() {
		if derr := prov.Delete(context.Background(), key); derr != nil {
			log.Printf("migration %s: R2 transfer object cleanup failed (%s): %v", srv.UUID, key, derr)
		}
	}()

	// Source uploads its staged archive to R2.
	if err := o.queue.SendMigratePushR2Command(ctx, sourceNode.Token, srv.UUID, putURL); err != nil {
		return fmt.Errorf("queue migrate_push_r2: %w", err)
	}
	if phase, nerr := o.waitForNodePhase(ctx, sourceNode.Token, srv.UUID, "pushed", migrationR2PhaseTimeout); phase != "pushed" {
		return fmt.Errorf("source R2 upload failed (phase=%s): %s", phase, nerr)
	}

	// Target downloads from R2, verifies the hash, extracts. Reports "transferred".
	if err := o.queue.SendMigratePullR2Command(ctx, targetNode.Token, srv.UUID, getURL, expectedSha256, expectedSize); err != nil {
		return fmt.Errorf("queue migrate_pull_r2: %w", err)
	}
	if phase, nerr := o.waitForNodePhase(ctx, targetNode.Token, srv.UUID, "transferred", migrationR2PhaseTimeout); phase != "transferred" {
		return fmt.Errorf("target R2 download failed (phase=%s): %s", phase, nerr)
	}
	return nil
}

// nodeMeta mirrors the node-owned dylaris:migration:<uuid>:meta JSON.
type nodeMeta struct {
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	SourceNodeID string `json:"sourceNodeID"`
	StagedAt     int64  `json:"stagedAt"`
}

func (o *MigrationOrchestrator) readMeta(ctx context.Context, nodeToken, serverUUID string) (nodeMeta, error) {
	key := queue.MigrationMetaKey(nodeToken, serverUUID)
	raw, err := o.redis.Get(ctx, key).Result()
	if err != nil {
		return nodeMeta{}, err
	}
	var m nodeMeta
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nodeMeta{}, err
	}
	if m.SHA256 == "" {
		return nodeMeta{}, fmt.Errorf("meta missing sha256")
	}
	return m, nil
}

// currentPlayerCount reads the live player count for a server. Thin wrapper
// over playerCountFromStats so the orchestrator and the rebalance worker share
// one parser.
func (o *MigrationOrchestrator) currentPlayerCount(ctx context.Context, serverUUID string) int {
	return playerCountFromStats(ctx, o.redis, serverUUID)
}

// playerCountFromStats reads the latest entry of the per-server stats buffer
// stream and returns the player count, or 0 if unreadable. The buffer entry's
// "data" field is a JSON StatsPayload with a lowercase "players" field.
func playerCountFromStats(ctx context.Context, rdb *redis.Client, serverUUID string) int {
	key := fmt.Sprintf("dylaris:server:%s:stats:buffer", serverUUID)
	msgs, err := rdb.XRevRangeN(ctx, key, "+", "-", 1).Result()
	if err != nil || len(msgs) == 0 {
		return 0
	}
	raw, ok := msgs[0].Values["data"].(string)
	if !ok || raw == "" {
		return 0
	}
	var p struct {
		Players int `json:"players"`
	}
	if json.Unmarshal([]byte(raw), &p) != nil {
		return 0
	}
	return p.Players
}

// writeStatus writes the orchestration progress record (best-effort).
// reResolvePinningForTarget re-evaluates a server's CPU pinning for the node it
// just moved to. auto is recomputed on the target topology; manual is kept only
// when the target hardware signature matches the source and the cpuset is still
// valid, otherwise it resets to shared so the operator re-pins intentionally on
// the new hardware. shared is unchanged. Returns the effective cpuset to apply
// and whether the server is pinned (so the caller recreates instead of plain start).
func (o *MigrationOrchestrator) reResolvePinningForTarget(ctx context.Context, srv *models.Server, source, target *models.Node) (string, bool) {
	if o.cpuPinning == nil {
		return target.CpusetCpus, false
	}
	switch srv.CPUPinningMode {
	case "auto":
		cs, _ := o.cpuPinning.AutoCpuset(ctx, target.Token, target.ID, srv.ID, srv.CPULimit, target.CpusetCpus)
		if err := o.store.UpdateServerCPUPinning(srv.ID, "auto", cs); err != nil {
			log.Printf("migration %s: persist recomputed auto cpuset failed: %v", srv.UUID, err)
		}
		srv.Cpuset = cs
		if cs == "" {
			return target.CpusetCpus, false // target has not reported a topology yet
		}
		log.Printf("migration %s: auto cpuset recomputed for target %s -> %s", srv.UUID, target.Name, cs)
		return cs, true
	case "manual":
		srcSig, _ := o.redis.Get(ctx, fmt.Sprintf("dylaris:node:%s:cpu:sig", source.Token)).Result()
		tgtSig, _ := o.redis.Get(ctx, fmt.Sprintf("dylaris:node:%s:cpu:sig", target.Token)).Result()
		sameHW := srcSig != "" && srcSig == tgtSig
		if sameHW && o.cpuPinning.ValidateCpuset(ctx, target.Token, srv.Cpuset, target.CpusetCpus) == nil {
			return srv.Cpuset, true // identical hardware -> keep the explicit pin
		}
		if err := o.store.UpdateServerCPUPinning(srv.ID, "shared", ""); err != nil {
			log.Printf("migration %s: reset manual pinning failed: %v", srv.UUID, err)
		}
		srv.CPUPinningMode = "shared"
		srv.Cpuset = ""
		log.Printf("migration %s: manual cpuset reset to shared (target %s hardware differs)", srv.UUID, target.Name)
		return target.CpusetCpus, false
	default: // shared / empty
		return target.CpusetCpus, false
	}
}

func (o *MigrationOrchestrator) writeStatus(ctx context.Context, serverUUID, phase, errMsg string, sourceNodeID, targetNodeID int, reason string, startedAt time.Time) {
	st := orchestrationStatus{
		Phase:        phase,
		Error:        errMsg,
		SourceNodeID: sourceNodeID,
		TargetNodeID: targetNodeID,
		Reason:       reason,
		StartedAt:    startedAt.Unix(),
		UpdatedAt:    time.Now().Unix(),
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	key := fmt.Sprintf("dylaris:migration:%s:orchestration", serverUUID)
	if err := o.redis.Set(ctx, key, data, orchestrationStatusTTL).Err(); err != nil {
		log.Printf("migration %s: failed to write orchestration status: %v", serverUUID, err)
	}
}

// migrationSourceLANIPs is what a migrate_in is told about the source's LAN.
//
// A move where either end sits outside the datacenter - a customer's machine
// (owned) or an External node - gets the source's LAN addresses, so a same-LAN
// move pulls directly and any other one answers need_remote and falls back to
// the object-storage transfer. Such a machine is reached only through a
// WireGuard spoke, and the overlay endpoint the node publishes for the pull is
// not reachable across it: with an empty list the target would try that
// endpoint, fail, and the move would roll back every time.
//
// Platform to platform stays overlay-only (nil). IsExternal reads a tag the node
// reports itself, which is acceptable here: it only chooses a transport, and a
// wrong answer costs a failed move, never access.
func migrationSourceLANIPs(source, target *models.Node) []string {
	if source.OwnerID != nil || target.OwnerID != nil || source.IsExternal() || target.IsExternal() {
		return source.PrivateIPs
	}
	return nil
}
