package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"strings"
	"time"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"

	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"
)

// BackupScheduler is leader-gated: it ticks every minute, lists due backup
// jobs, dispatches them to nodes via Redis, and advances next_run_at. It
// pushes commands directly to Redis (mirroring handlers.startBackupRun
// without the HTTP-specific bits) to avoid an import cycle with handlers.
type BackupScheduler struct {
	store    store.Store
	redis    *redis.Client
	queue    *QueueService      // publishes backup_run to the node's :cmds stream (BC1)
	registry *nodegrpc.Registry // optional — required only for node-local retention deletes
	leader   leader.Election
	// coreStorage opens the shared Core file storage as a backup backend.
	// Optional; required only for jobs whose storage row is "core-storage".
	coreStorage func(subPrefix string) (backupstorage.Storage, error)
	// connection opens a saved storage connection as a backup backend.
	// Optional; required only for jobs whose storage row is "connection".
	connection func(connectionID int, prefix string) (backupstorage.Storage, error)
	// storeEnabled mirrors config.StoreEnabled and reaches the backup
	// allowance, which has to tell "no billing plane exists" from "this owner
	// holds no entitlement" - the same distinction BillingLifecycleService
	// already carries for the over-limit sweep. Zero value false means
	// self-host, where the operator's Settings, Backups allowance is the answer.
	storeEnabled bool
}

// SetStoreEnabled mirrors config.StoreEnabled into the scheduler. Without it the
// cron path would resolve a different allowance from the manual path for the
// same server, which is exactly the split this rebuild removes.
func (b *BackupScheduler) SetStoreEnabled(v bool) { b.storeEnabled = v }

func NewBackupScheduler(s store.Store, r *redis.Client, q *QueueService) *BackupScheduler {
	return &BackupScheduler{store: s, redis: r, queue: q}
}

// SetRegistry wires the gRPC mesh registry so the retention sweep can call
// into Nodes to delete node-local backups. Optional: a scheduler running
// without a registry simply logs and skips node-local deletes (the Node-side
// retention pass still trims its own folder).
func (b *BackupScheduler) SetRegistry(reg *nodegrpc.Registry) {
	b.registry = reg
}

// SetCoreStorage wires the builder that opens the shared Core file storage as
// a backup backend. Without it, backupstorage.Open refuses every job whose
// storage row is "core-storage", so the reaper's probe could never reach one:
// it reported "could not be determined" for the whole provider rather than
// present-or-absent, which is the answer the reaper exists to give.
//
// Supplied from main.go, which owns the AppState the builder needs. Optional,
// like SetRegistry: a scheduler without it still reaps, it just cannot inspect
// core-storage-backed archives.
func (b *BackupScheduler) SetCoreStorage(fn func(subPrefix string) (backupstorage.Storage, error)) {
	b.coreStorage = fn
}

// SetConnection wires the builder that opens a saved storage connection as a
// backup backend. Same shape and same reason as SetCoreStorage: without it,
// backupstorage.Open refuses every job whose storage row is "connection", which
// covers dispatch, the retention delete and the reaper's probe alike.
func (b *BackupScheduler) SetConnection(fn func(connectionID int, prefix string) (backupstorage.Storage, error)) {
	b.connection = fn
}

// SetLeader wires the leader-election gate. Without it the scheduler ticks
// + processes Pub/Sub messages on every Core (single-instance dev mode).
// With it, only the elected Core does the work — followers still subscribe
// to Pub/Sub but ignore the messages, which keeps the message shape on the
// node side unchanged. Conversion to XReadGroup for true competing-consumer
// semantics is a follow-up.
func (b *BackupScheduler) SetLeader(l leader.Election) { b.leader = l }

// Start runs the scheduler tick loop until ctx is cancelled. Polls every
// 60s — backup work isn't latency-sensitive and frequent polling would just
// hammer the DB. Also subscribes to backup result messages from nodes.
func (b *BackupScheduler) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		if b.leader == nil || b.leader.IsLeader() {
			b.tick(ctx) // immediate first run only on the leader
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if b.leader != nil && !b.leader.IsLeader() {
					continue
				}
				b.tick(ctx)
			}
		}
	}()
	go b.consumeResults(ctx)
	go b.consumeRestoreResults(ctx)
}

// reporterMatchesServer reports whether the node that published on channel is
// the node that actually hosts serverID.
//
// This is the Core-side half of the per-node channel scoping (see
// queue.BackupResultsChannel). Redis already refuses a cross-node PUBLISH
// through the node's ACL, so reaching a mismatch here means either an ACL that
// was never applied or a credential with wider reach than a node's - both of
// which are exactly the cases a second, independent check is for.
//
// A channel the pattern matched but that carries no token, or a server/node this
// Core cannot load, is UNATTRIBUTABLE and returns false. Failing closed loses a
// result, which the reaper later closes as an unverified run; failing open lets
// an unattributable message write another tenant's backup_runs row.
func (b *BackupScheduler) reporterMatchesServer(channel string, serverID int, what string) bool {
	token, ok := queue.NodeTokenFromBackupChannel(channel)
	if !ok {
		log.Printf("%s: dropping a message on unattributable channel %q", what, channel)
		return false
	}
	srv, err := b.store.GetServerByID(serverID)
	if err != nil || srv == nil {
		logErrf("backup-scheduler", "%s: dropping a message from node %q: server %d could not be loaded: %v", what, token, serverID, err)
		return false
	}
	node, err := b.store.GetNodeByID(srv.NodeID)
	if err != nil || node == nil {
		logErrf("backup-scheduler", "%s: dropping a message from node %q: node %d could not be loaded: %v", what, token, srv.NodeID, err)
		return false
	}
	if node.Token != token {
		log.Printf("%s: DROPPED a message from node %q for server %d, which is hosted by a different node - a node may only report on its own runs",
			what, token, serverID)
		return false
	}
	return true
}

// consumeRestoreResults listens on every node's own restore channel and updates
// the backup_restores row with the outcome reported by the node that hosts the
// server. Pattern subscription, so the channel name identifies the publisher;
// see queue.BackupRestoresChannel.
func (b *BackupScheduler) consumeRestoreResults(ctx context.Context) {
	pubsub := b.redis.PSubscribe(ctx, queue.BackupRestoresPattern)
	defer pubsub.Close()
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// Leader-gate: Pub/Sub broadcasts to every
			// subscriber, so without this guard each Core writes the
			// same backup_restores row N times. Only the leader updates.
			if b.leader != nil && !b.leader.IsLeader() {
				continue
			}
			var result struct {
				RestoreID int    `json:"restoreId"`
				Status    string `json:"status"`
				Error     string `json:"error"`
			}
			if err := json.Unmarshal([]byte(msg.Payload), &result); err != nil {
				logErrf("backup-scheduler", "restore result: decode failed: %v", err)
				continue
			}
			if result.RestoreID == 0 {
				continue
			}
			// The restore row names the server directly, so the reporting node is
			// checked against it before anything is written.
			restore, err := b.store.GetBackupRestore(result.RestoreID)
			if err != nil || restore == nil {
				log.Printf("restore result: restore %d not found", result.RestoreID)
				continue
			}
			if !b.reporterMatchesServer(msg.Channel, restore.ServerID, "restore result") {
				continue
			}
			completed := time.Time{}
			if result.Status == "success" || result.Status == "failed" {
				completed = time.Now()
			}
			if err := b.store.UpdateBackupRestoreStatus(result.RestoreID, result.Status, result.Error, completed); err != nil {
				logErrf("backup-scheduler", "restore result: update failed for id=%d: %v", result.RestoreID, err)
			}
			if result.Status == "success" {
				b.restoreInstalls(restore.RunID, restore.ServerID)
				b.restoreMods(restore.RunID, restore.ServerID)
			}
		}
	}
}

// consumeResults listens on every node's own results channel and updates the
// backup_runs row when the node that hosts the server finishes (success or
// failure). Also prunes old runs from storage when retention limits are
// exceeded - which is why the attribution check below is not merely tidy: a
// forged "success" on a foreign run fires enforceRetention and DELETES that
// job's older archives.
func (b *BackupScheduler) consumeResults(ctx context.Context) {
	pubsub := b.redis.PSubscribe(ctx, queue.BackupResultsPattern)
	defer pubsub.Close()
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// Leader-gate: same Pub/Sub broadcast issue as in
			// consumeRestoreResults — only the leader processes results.
			if b.leader != nil && !b.leader.IsLeader() {
				continue
			}
			var result struct {
				RunID     int    `json:"runId"`
				Status    string `json:"status"`
				Error     string `json:"error"`
				SizeBytes int64  `json:"sizeBytes"`
			}
			if err := json.Unmarshal([]byte(msg.Payload), &result); err != nil {
				logErrf("backup-scheduler", "backup result: decode failed: %v", err)
				continue
			}
			run, err := b.store.GetBackupRun(result.RunID)
			if err != nil {
				log.Printf("backup result: run %d not found", result.RunID)
				continue
			}
			// Attribution BEFORE the write, and before the retention prune the
			// write can trigger. The run points at a job, the job at a server, the
			// server at the node that is allowed to report on it.
			job, err := b.store.GetBackupJob(run.JobID)
			if err != nil || job == nil {
				log.Printf("backup result: job %d for run %d not found", run.JobID, result.RunID)
				continue
			}
			if !b.reporterMatchesServer(msg.Channel, job.ServerID, "backup result") {
				continue
			}
			b.store.UpdateBackupRunStatus(result.RunID, result.Status, result.Error, result.SizeBytes, run.StorageKey, time.Now())
			if result.Status == "success" {
				b.snapshotInstalls(result.RunID, job)
				b.enforceRetention(ctx, run.JobID)
			}
		}
	}
}

// enforceRetention deletes successful runs that exceed the job's retention
// count, both from the DB and from the underlying storage provider.
func (b *BackupScheduler) enforceRetention(ctx context.Context, jobID int) {
	job, err := b.store.GetBackupJob(jobID)
	if err != nil {
		return
	}
	pruned, err := b.store.PruneOldBackupRuns(jobID, job.RetentionCount)
	if err != nil || len(pruned) == 0 {
		return
	}
	// A nil StorageID means the platform default, not "no storage". Reading it as
	// the latter left `storage` nil and returned HERE - after PruneOldBackupRuns
	// above had already deleted the rows. So on a job using the panel's default
	// storage option, every retention cycle dropped the records and left the
	// archives behind, permanently and without a log line. That is the failure
	// mode with no ceiling: it repeats on every prune, for every such job, and
	// nothing afterwards knows those objects exist.
	owner := BackupJobOwner(b.store, job.ServerID)
	// Best-effort delete from storage — DB rows are already gone. That ordering
	// is pre-existing: PruneOldBackupRuns both prunes and reports, so a failure
	// here still orphans that one archive. Resolving the storage correctly turns
	// "always orphans" into "orphans on a transient fault", which is the part
	// worth fixing without restructuring the store call.
	//
	// Resolved PER RUN, not once for the job: a job whose storage was changed
	// has older archives sitting somewhere else, and deleting them from the
	// job's current storage would delete nothing while reporting success.
	for _, run := range pruned {
		storage, err := ResolveRunStorage(b.store, &run, job.StorageID, owner)
		if err != nil {
			logErrf("backup-scheduler", "retention prune: job %d — cannot resolve storage for run %d: %v; the archive is now untracked",
				jobID, run.ID, err)
			continue
		}
		if err := b.deleteStorageObject(ctx, storage, run.StorageKey); err != nil {
			logErrf("backup-scheduler", "retention prune: %s delete failed: %v", run.StorageKey, err)
		}
	}
}

// storageDeps is the single place the scheduler's backup-storage dependencies
// are assembled, so a provider that needs one of them cannot work on one code
// path and be refused on another. That is what happened to core-storage: the
// retention delete and the reaper each built their own Deps literal without a
// CoreStorage builder — and then again to "connection", which had no builder
// here at all, so retention could never delete an expired archive from a saved
// storage connection.
func (b *BackupScheduler) storageDeps() backupstorage.Deps {
	return backupstorage.Deps{
		Registry:    b.registry,
		NodeStore:   b.store,
		CoreStorage: b.coreStorage,
		Connection:  b.connection,
	}
}

// deleteStorageObject opens the configured backend and deletes a single
// object. Used by the retention pass after a successful run.
func (b *BackupScheduler) deleteStorageObject(ctx context.Context, bs *models.BackupStorage, key string) error {
	deps := b.storageDeps()
	provider, err := backupstorage.Open(ctx, bs, deps)
	if err != nil {
		return err
	}
	return provider.Delete(ctx, key)
}

func (b *BackupScheduler) tick(ctx context.Context) {
	if b.store == nil || b.redis == nil {
		return
	}
	now := time.Now()
	jobs, err := b.store.ListDueBackupJobs(now)
	if err != nil {
		logErrf("backup-scheduler", "ListDueBackupJobs error: %v", err)
		return
	}
	for _, job := range jobs {
		if err := b.dispatch(ctx, job); err != nil {
			logErrf("backup-scheduler", "job %d dispatch failed: %v", job.ID, err)
		}
	}

	// Runs whose result never came back. Deliberately AFTER dispatch: reaping
	// is cleanup and must never delay this tick's real work.
	b.reapAbandonedRuns(ctx, now)
}

// How long a run may sit at "running" before it is treated as abandoned.
//
// Generous on purpose. The window has to clear the slowest legitimate backup -
// archiving tens of GB on a busy node and uploading it over a home BYON uplink -
// because the cost of being wrong is a scary "failed" against a backup that
// was actually still working.
//
// Being wrong is also recoverable, which is what allows a single fixed number
// here instead of something adaptive: consumeResults updates by run id without
// checking the current status, so a result that finally arrives after the
// reaper gave up overwrites the verdict with the truth, retention included.
const backupRunAbandonedAfter = 6 * time.Hour

// How many abandoned runs one tick will close. Each one may open a storage
// connection, and the first sweep after this ships can meet a backlog that has
// been accumulating since the deployment was installed.
const backupReapBatchSize = 25

// reapAbandonedRuns closes runs whose result never arrived.
//
// It resolves them to "failed" and never to "success", even when the archive
// turns out to be sitting in storage. Core has no confirmation from the node -
// no reported size, no completion signal - so calling such a run successful
// would present an unverified archive as a backup somebody can rely on. That is
// the one error in this whole path that actually hurts, because it is only
// discovered during a restore. The message says what was found instead, and an
// operator can act on that.
//
// Nothing is deleted here for the same reason: an archive that may be a
// complete backup is not something a cleanup routine should remove on a guess.
func (b *BackupScheduler) reapAbandonedRuns(ctx context.Context, now time.Time) {
	runs, err := b.store.ListAbandonedBackupRuns(now.Add(-backupRunAbandonedAfter), backupReapBatchSize)
	if err != nil {
		logErrf("backup-scheduler", "listing abandoned runs failed: %v", err)
		return
	}
	for _, run := range runs {
		size, detail := b.describeAbandonedRun(ctx, run)
		age := now.Sub(run.StartedAt).Round(time.Minute)
		message := fmt.Sprintf("No result was received from the node within %s. %s", age, detail)
		if err := b.store.UpdateBackupRunStatus(run.ID, "failed", message, size, run.StorageKey, now); err != nil {
			logErrf("backup-scheduler", "could not close abandoned run %d: %v", run.ID, err)
			continue
		}
		log.Printf("backup-scheduler: closed abandoned run %d (job %d, started %s ago): %s", run.ID, run.JobID, age, detail)
	}
}

// describeAbandonedRun reports whether an archive turned up where the run was
// going to write one, and returns its size alongside a sentence for the run's
// error message.
//
// The size is recorded even though the run is failed, and it now counts toward
// the storage quota. The object is real bytes on the backend, so the quota
// queries (store/billing.go BackupBytesByOwner, store/traffic.go
// TenantBackupBytes) count any run with a nonzero size, not just successes. A
// node-failed run is always size 0, so this only ever picks up the archives the
// reaper actually found. It is also the most useful single number for deciding
// whether the archive is worth investigating.
func (b *BackupScheduler) describeAbandonedRun(ctx context.Context, run models.BackupRun) (int64, string) {
	if run.StorageKey == "" {
		return 0, "The run never recorded a storage key, so no archive was written."
	}

	obj, err := b.statBackupObject(ctx, run)
	switch {
	case err == nil:
		return obj.Size, fmt.Sprintf(
			"An archive of %d bytes is present at %s. The node never confirmed it, so it is UNVERIFIED - check it before relying on it. It is left in place and counts against the storage quota; delete this run to reclaim the space.",
			obj.Size, run.StorageKey)
	case errors.Is(err, fs.ErrNotExist):
		return 0, fmt.Sprintf("No archive was written to %s, so the backup did not complete.", run.StorageKey)
	default:
		// Storage itself could not answer. Saying "no archive exists" here
		// would be a claim the code cannot make.
		//
		// The error is LOGGED, not stored. This message is rendered to anyone
		// holding backups.read on the server - a tenant, not an operator -
		// while the backend's endpoint and bucket live behind settings.read.
		// A transport failure from the S3 SDK carries the full request URL, so
		// pasting %v here would hand an internal hostname, bucket name and
		// often an internal IP to exactly the audience the settings boundary
		// keeps them from. The storage key is already in the API response, so
		// naming it costs nothing.
		logErrf("backup-scheduler", "could not determine whether run %d wrote an archive at %s: %v", run.ID, run.StorageKey, err)
		return 0, fmt.Sprintf("Whether an archive exists at %s could not be determined. The Core log has the reason.", run.StorageKey)
	}
}

// statBackupObject opens the job's configured backend and stats the run's key.
func (b *BackupScheduler) statBackupObject(ctx context.Context, run models.BackupRun) (backupstorage.Object, error) {
	job, err := b.store.GetBackupJob(run.JobID)
	if err != nil {
		return backupstorage.Object{}, fmt.Errorf("load job: %w", err)
	}
	// Same resolution runJob uses a few lines below. Refusing on a nil
	// StorageID meant a job on the default storage could not even be checked
	// for whether its archive existed, so every one of its runs was reported
	// as UNVERIFIED.
	// The RUN's own storage where it has one: an archive written before the job
	// was pointed somewhere else is still where it was written, and statting the
	// job's current storage would report it missing.
	bs, err := ResolveRunStorage(b.store, &run, job.StorageID, BackupJobOwner(b.store, job.ServerID))
	if err != nil {
		return backupstorage.Object{}, fmt.Errorf("resolve storage: %w", err)
	}
	provider, err := backupstorage.Open(ctx, bs, b.storageDeps())
	if err != nil {
		return backupstorage.Object{}, fmt.Errorf("open storage: %w", err)
	}
	return provider.Stat(ctx, run.StorageKey)
}

func (b *BackupScheduler) dispatch(ctx context.Context, job models.BackupJob) error {
	srv, err := b.store.GetServerByID(job.ServerID)
	if err != nil {
		return fmt.Errorf("server lookup: %w", err)
	}
	node, err := b.store.GetNodeByID(srv.NodeID)
	if err != nil {
		return fmt.Errorf("node lookup: %w", err)
	}

	// Resolve storage through the one chain: job -> the owner's own default ->
	// the platform default. This used to hand-roll two of the three steps, so a
	// tenant who had connected their own bucket had every SCHEDULED backup
	// written to ours anyway - the manual path honoured their choice and the
	// cron path did not, which is the sort of difference nobody notices until
	// the bill or the restore.
	storage, err := ResolveJobStorage(b.store, job.StorageID, srv.OwnerID)
	if err != nil {
		return fmt.Errorf("resolve storage: %w", err)
	}

	// Platform backup allowance: skip the scheduled run once the owner is at or
	// over it. The next_run is still advanced below so we don't re-check on a
	// tight loop; the run resumes when usage drops or the allowance rises.
	//
	// Skipped entirely when the archive is headed for a storage the TENANT
	// connected: those bytes are theirs, they are not counted by
	// BackupBytesByOwner, and deleteTenantBackups already refuses to touch them
	// at any retention deadline. Charging them against our ceiling was the one
	// place that did not take that distinction.
	if exceeded, used, quota := BackupAllowanceExceeded(b.store, srv.OwnerID, b.storeEnabled, storage); exceeded {
		log.Printf("backup-scheduler: job %d skipped — quota reached (%d/%d GB)", job.ID, used/(1<<30), quota/(1<<30))
		next := ComputeBackupNextRun(job.Schedule, time.Now())
		if next != nil {
			b.store.SetBackupJobScheduled(job.ID, time.Now(), *next)
		}
		return nil
	}
	// Per-server node-local cap - the same gate startBackupRun takes. Cron is
	// the path that actually fills a disk: nobody is watching it, and it runs
	// again every interval forever.
	// No administrator exemption here, deliberately, unlike the allowance above:
	// this one bounds a real disk on the MC host, and a full disk takes every
	// server on that host down with it, including other people's.
	if exceeded, used, quota := NodeLocalBackupQuotaExceeded(b.store, b.registry, srv); exceeded {
		log.Printf("backup-scheduler: job %d skipped — per-server backup quota reached (%.1f/%.1f GB)",
			job.ID, float64(used)/(1<<30), float64(quota)/(1<<30))
		next := ComputeBackupNextRun(job.Schedule, time.Now())
		if next != nil {
			b.store.SetBackupJobScheduled(job.ID, time.Now(), *next)
		}
		return nil
	}

	storageKey := fmt.Sprintf("backups/%s/job-%d/%s.tar.gz", srv.UUID, job.ID, time.Now().UTC().Format("20060102-150405"))
	runID, err := b.store.CreateBackupRun(&models.BackupRun{
		JobID:  job.ID,
		Status: "running",
		// Recorded now, from the storage this dispatch actually resolved, so the
		// archive stays findable if the job is pointed elsewhere later.
		StorageID:  &storage.ID,
		StorageKey: storageKey,
	})
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}

	if b.queue == nil {
		b.store.UpdateBackupRunStatus(runID, "failed", "queue unavailable", 0, "", time.Now())
		return fmt.Errorf("queue unavailable")
	}

	storageCfgJSON, presignedPut, err := PrepareNodeStorage(ctx, b.store, storage, node, storageKey, "put", b.storageDeps())
	if err != nil {
		// An indirection target Core could not resolve. Failing here names the
		// storage row; dispatching anyway would surface as the node's opaque
		// "unknown provider" two hops later.
		b.store.UpdateBackupRunStatus(runID, "failed", err.Error(), 0, "", time.Now())
		return err
	}
	subServer := ""
	if job.SubServer != nil {
		subServer = *job.SubServer
	}
	payload := map[string]interface{}{
		"action":          "backup_run",
		"runId":           runID,
		"jobId":           job.ID,
		"serverUuid":      srv.UUID,
		"subServer":       subServer,
		"includePatterns": job.IncludePatterns,
		"excludePatterns": job.ExcludePatterns,
		"storageKey":      storageKey,
		"storage":         json.RawMessage(storageCfgJSON),
		"presignedPutUrl": presignedPut,
	}
	// Publish to the node's durable :cmds stream (BC1) instead of RPush to the
	// retired dylaris:node:<token>:queue list, which nothing reads anymore.
	if err := b.queue.SendRawCommand(ctx, node.Token, payload); err != nil {
		b.store.UpdateBackupRunStatus(runID, "failed", "queue push: "+err.Error(), 0, "", time.Now())
		return err
	}

	// Advance next_run_at so we don't re-dispatch on the next tick.
	next := ComputeBackupNextRun(job.Schedule, time.Now())
	if next != nil {
		b.store.SetBackupJobScheduled(job.ID, time.Now(), *next)
	} else {
		b.store.SetBackupJobScheduled(job.ID, time.Now(), time.Time{})
	}

	return nil
}

// ValidBackupSchedule reports whether a schedule string is one the scheduler can act
// on. Defined in terms of ComputeNextRun rather than re-parsing, so the answer
// and the behaviour can never disagree - which is the whole failure it exists
// to stop: the parser was always right about "banana", the caller just stored
// the job anyway and it silently never ran.
func ValidBackupSchedule(schedule string) bool {
	schedule = strings.TrimSpace(schedule)
	if schedule == "" || schedule == "manual" {
		return true
	}
	return ComputeBackupNextRun(schedule, time.Now()) != nil
}

// ComputeBackupNextRun parses the schedule expressions the backup scheduler
// understands: "manual"/empty and "every Nh" / "every Nd". Anything else
// returns nil, meaning "I cannot schedule this" - callers must act on that
// rather than store the job regardless.
//
// It used to have an identical twin in handlers/backup.go, "duplicated here to
// keep services free of any handlers dependency" - but the dependency runs the
// other way (handlers already imports services), so the copy bought nothing and
// gave a validator two definitions to drift between.
func ComputeBackupNextRun(schedule string, from time.Time) *time.Time {
	if schedule == "" || schedule == "manual" {
		return nil
	}
	var n int
	var unit string
	if _, err := fmt.Sscanf(schedule, "every %d%s", &n, &unit); err != nil || n <= 0 {
		return nil
	}
	var d time.Duration
	switch unit {
	case "h":
		d = time.Duration(n) * time.Hour
	case "d":
		d = time.Duration(n) * 24 * time.Hour
	default:
		return nil
	}
	next := from.Add(d)
	return &next
}

// snapshotInstalls records how the archived sub-servers were installed.
//
// A backup captures files and says nothing about the install, so without this a
// restore left the records describing whatever was installed LAST: restore an
// archive taken under modpack version 3 while the record says 5, and the setup
// screen confidently shows the wrong pack and treats re-picking version 3 as a
// change.
//
// A job with no sub-server backs up the whole container, so every record for the
// server is captured; a scoped job captures the one it archives.
//
// Best-effort: the backup has already succeeded and must not be marked failed
// because a convenience snapshot could not be written.
func (b *BackupScheduler) snapshotInstalls(runID int, job *models.BackupJob) {
	var records []models.SubServerInstall
	if job.SubServer != nil && *job.SubServer != "" {
		rec, err := b.store.GetSubServerInstall(job.ServerID, *job.SubServer)
		if err != nil || rec == nil {
			return
		}
		records = []models.SubServerInstall{*rec}
	} else {
		all, err := b.store.ListSubServerInstalls(job.ServerID)
		if err != nil || len(all) == 0 {
			return
		}
		records = all
	}
	blob, err := json.Marshal(records)
	if err != nil {
		log.Printf("backup snapshot: marshal for run %d: %v", runID, err)
		return
	}
	if err := b.store.SetBackupRunInstallSnapshot(runID, string(blob)); err != nil {
		log.Printf("backup snapshot: run %d: %v", runID, err)
	}
}

// restoreMods puts the archived mod rows back after a restore.
//
// The files have just been replaced wholesale by an atomic directory swap, and
// until this ran the DATABASE kept describing whatever was installed last.
// Measured both ways before it existed: back up, install a mod, restore - the
// jar is gone and the row stays, so the panel lists a mod that is not there.
// Install a mod, back up, uninstall it, restore - the jar is back and the row is
// gone, so the mod runs and nothing in the panel knows about it. The second is
// the worse one, because a jar the panel cannot name is a jar nobody updates.
//
// A run with no manifest changes nothing, and that is not a gap to close later:
// every archive written before manifests existed has none, and clearing a
// server's mod list on the strength of a description that does not exist would
// replace a list that might be stale with no list at all.
//
// Scope matters. An entry with an EMPTY mod list still clears its sub-server -
// that is the archive saying "there were none" - while a sub-server the manifest
// does not mention is left alone, because the archive says nothing about it.
// Those two must not collapse into each other.
func (b *BackupScheduler) restoreMods(runID, serverID int) {
	run, err := b.store.GetBackupRun(runID)
	if err != nil || run == nil {
		return
	}
	m, ok := DecodeBackupManifest(run.Manifest)
	if !ok {
		return
	}
	for _, entry := range m.Mods {
		rows := ManifestModRows(entry.Mods)
		// serverID comes from the RESTORE, never from the archive: an archive can
		// be restored onto a different server, and an id read out of it would
		// rewrite whatever the backup was taken from. Same rule restoreInstalls
		// applies, for the same reason.
		if err := b.store.ReplaceServerMods(serverID, entry.SubServer, rows); err != nil {
			logErrf("backup-scheduler", "restore mods: server %d/%s: %v", serverID, entry.SubServer, err)
		}
	}
}

// restoreInstalls puts the archived install records back after a restore.
//
// Nothing to put back is the common case and NOT an error: every run from before
// this was captured has an empty snapshot, and the honest answer there is to
// leave the existing records alone rather than clear them. A restore that
// silently emptied them would trade a record that might be stale for no record
// at all, which reads on screen as "we never wrote this down".
func (b *BackupScheduler) restoreInstalls(runID, serverID int) {
	run, err := b.store.GetBackupRun(runID)
	if err != nil || run == nil || run.InstallSnapshot == "" {
		return
	}
	var records []models.SubServerInstall
	if err := json.Unmarshal([]byte(run.InstallSnapshot), &records); err != nil {
		log.Printf("restore installs: run %d snapshot is unreadable: %v", runID, err)
		return
	}
	for _, rec := range records {
		// The server ID comes from the RESTORE, not from the snapshot: an archive
		// can be restored onto a different server, and writing the recorded id
		// back would attach the record to whatever the backup came from.
		rec.ServerID = serverID
		if err := b.store.UpsertSubServerInstall(rec); err != nil {
			log.Printf("restore installs: server %d/%s: %v", serverID, rec.SubServerName, err)
		}
	}
}
