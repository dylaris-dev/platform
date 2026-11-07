package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dylaris-pkg/migration"
	"dylaris-pkg/queue"
	"dylaris-pkg/validate"

	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"
)

// Auto-move (Wave 2b) command handlers. Migration is GATEWAY-ONLY: in gateway
// mode the node binds no host ports (see docker_mgr.go), so a move only relocates
// the server's data directory. No host port is allocated or released here.
//
// Redis keys this file owns:
//   dylaris:migration:<uuid>:meta    JSON {sha256,size,sourceNodeID,stagedAt}, TTL ~1h
//   dylaris:migration:<uuid>:status  JSON {phase,error?,bytes?}, TTL ~1h
// The endpoint key dylaris:migration:endpoint:<nodeID> is owned by migration_server.go.

const (
	migrationStagingDir = ".dylaris-migration"
	migrationMetaTTL    = time.Hour
	migrationStatusTTL  = time.Hour
)

// migrationMeta is published after staging so core can read the archive hash
// (which it forwards to the target as the Pull integrity check).
type migrationMeta struct {
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	SourceNodeID string `json:"sourceNodeID"`
	StagedAt     int64  `json:"stagedAt"` // unix seconds
}

// migrationStatus tracks a move's progress for core/orchestrator polling.
// Phases: staged (source archived) | need_remote (target can't reach source LAN,
// orchestrator should use the R2 fallback) | pushed (source uploaded to R2) |
// transferred (target has the verified copy) | error.
type migrationStatus struct {
	Phase string `json:"phase"`
	Error string `json:"error,omitempty"` // populated only on phase=error
	// Bytes is how far the work has got: the staged archive's size on
	// phase=staging, the bytes sent on pushing, the bytes written on
	// transferring. Core extends its wait while it grows; an older Core ignores
	// both.
	Bytes int64 `json:"bytes,omitempty"`
}

// stagedArchivePath returns the staged-zip path for a server given its storage
// path: <storagePath>/.dylaris-migration/<uuid>.zip.
func stagedArchivePath(storagePath, serverUUID string) string {
	return filepath.Join(storagePath, migrationStagingDir, serverUUID+".zip")
}

// migrationAbortedPath records the attempt an abort stopped while it was
// staging. The abort count is memory only, and the abort is acked: after a
// node restart the redelivered migrate_out of that attempt would otherwise
// stage a full copy nothing removes.
func migrationAbortedPath(storagePath, serverUUID string) string {
	return filepath.Join(storagePath, migrationStagingDir, serverUUID+".aborted")
}

// migrationOriginPath is the stamp migrate_out drops next to the staged archive,
// naming the node that staged it.
// migrationAbortIssuedPath holds when Core queued the newest abort (unix
// seconds, Core's clock). A migrate_out queued before it is that abort's even
// when the two run concurrently after a restart and the abort finishes before
// the run registers: the count and migrateOutLatest then miss each other.
func migrationAbortIssuedPath(storagePath, serverUUID string) string {
	return filepath.Join(storagePath, migrationStagingDir, serverUUID+".abort-issued")
}

// abortedAfter reports whether an abort queued after issuedAt was recorded.
// 0 is a Core that stamps nothing, which no abort can be ordered against.
func abortedAfter(storagePath, serverUUID string, issuedAt int64) bool {
	if issuedAt == 0 {
		return false
	}
	b, err := os.ReadFile(migrationAbortIssuedPath(storagePath, serverUUID))
	if err != nil {
		return false
	}
	at, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return err == nil && at > issuedAt
}

func migrationOriginPath(storagePath, serverUUID string) string {
	return filepath.Join(storagePath, migrationStagingDir, serverUUID+".origin")
}

// sharedStorageOwner returns the OTHER node that staged this migration into one
// of our own storage paths, or "" when the storage is genuinely ours alone.
//
// Two nodes backed by the same network share are indistinguishable by path: both
// legitimately resolve to the same string (/storage/<uuid>), whether or not the
// bytes behind it are shared. So instead of comparing paths we prove sameness
// positively - a stamp sitting on OUR disk that names a DIFFERENT node can only
// mean that node's storage is our storage.
//
// It cannot false-positive on a stale stamp: a leftover from an earlier
// migration out of this node names this node and is ignored.
func sharedStorageOwner(storage *StorageManager, serverUUID string) string {
	for _, p := range storage.Paths() {
		data, err := os.ReadFile(migrationOriginPath(p, serverUUID))
		if err != nil {
			continue
		}
		if owner := strings.TrimSpace(string(data)); owner != "" && owner != nodeID {
			return owner
		}
	}
	return ""
}

// refuseIfStorageShared aborts a transfer whose source shares our storage.
//
// Such a "move" is not a move: source and target resolve to the same directory,
// so the pull overwrites the live server with a copy of itself and the source's
// cleanup then deletes it out from under the running container - total data loss
// reported as a successful migration. Refuse before anything is written, so the
// orchestrator fails the move with no cutover.
func refuseIfStorageShared(ctx context.Context, rdb *redis.Client, storage *StorageManager, nodeToken, serverUUID, progressID, cmd string) bool {
	owner := sharedStorageOwner(storage, serverUUID)
	if owner == "" {
		return false
	}
	log.Printf("%s %s: refusing - storage is shared with source node %s", cmd, serverUUID, owner)
	setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", fmt.Sprintf(
		"shared storage: this node and %s write to the same storage backend, so moving would delete the server", owner))
	return true
}

// migrationArchivePathFor is the archivePathFor callback handed to
// StartMigrationServer. It resolves the server's storage path and returns the
// staged archive, ok=true only if that file actually exists. We serve a staged
// archive only — never the live server dir.
func migrationArchivePathFor(storage *StorageManager) func(serverUUID string) (string, bool) {
	return func(serverUUID string) (string, bool) {
		storagePath := storage.GetServerPath(serverUUID)
		if storagePath == "" {
			return "", false
		}
		p := stagedArchivePath(storagePath, serverUUID)
		if stat, err := os.Stat(p); err != nil || stat.IsDir() {
			return "", false
		}
		return p, true
	}
}

// setMigrationStatus writes the migration status key (best-effort).
func setMigrationStatus(ctx context.Context, rdb *redis.Client, nodeToken, serverUUID, phase, errMsg string) {
	writeMigrationStatus(ctx, rdb, nodeToken, serverUUID, migrationStatus{Phase: phase, Error: errMsg})
}

func writeMigrationStatus(ctx context.Context, rdb *redis.Client, nodeToken, serverUUID string, st migrationStatus) {
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	key := queue.MigrationStatusKey(nodeToken, serverUUID)
	if err := rdb.Set(ctx, key, data, migrationStatusTTL).Err(); err != nil {
		log.Printf("migrate: failed to write status for %s: %v", serverUUID, err)
	}
}

// handleMigrateOut (source side) stages the server's data directory as a zip
// and publishes its hash. The orchestrator guarantees the server is already
// stopped before this runs, so we archive the dir as-is.
func handleMigrateOut(ctx context.Context, rdb *redis.Client, storage *StorageManager, nodeToken, serverUUID, progressID string, issuedAt int64) {
	aborts := migrateAbortCount(serverUUID)
	// A cancelled attempt can still be archiving when the retry runs, and
	// both stamp the same path: only the latest run may remove the stamp.
	// Identity, not progressID: a Core that sends no attempt gives both the
	// same one. It carries the progress id for handleMigrateAbort.
	run := &progressID
	migrateOutLatest.Store(serverUUID, run)
	defer migrateOutLatest.CompareAndDelete(serverUUID, run)
	storagePath := storage.GetServerPath(serverUUID)
	if storagePath == "" {
		log.Printf("migrate_out %s: no storage path found", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "storage path not found")
		return
	}
	// Only a per-attempt id: an old Core sends none, and every retry of its
	// would read as the aborted one.
	if b, err := os.ReadFile(migrationAbortedPath(storagePath, serverUUID)); err == nil && progressID != serverUUID && string(b) == progressID {
		log.Printf("migrate_out %s: attempt %s was aborted, not staging it", serverUUID, progressID)
		return
	}
	if abortedAfter(storagePath, serverUUID, issuedAt) {
		log.Printf("migrate_out %s: an abort was queued after attempt %s, not staging it", serverUUID, progressID)
		return
	}
	srcDir := filepath.Join(storagePath, serverUUID)
	if stat, err := os.Stat(srcDir); err != nil || !stat.IsDir() {
		log.Printf("migrate_out %s: server dir missing at %s", serverUUID, srcDir)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "server directory missing")
		return
	}
	// Beam and SFTP lose their write rights only on their next access check,
	// seconds after Core marks the server migrating: a write landing while the
	// tree is read is missing from the copy or torn in it, and the source is
	// deleted after cutover. Refused now, or dropped once the archive is read.
	writesSince, err := storage.watchWrites(serverUUID)
	if err != nil {
		log.Printf("migrate_out %s: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", err.Error())
		return
	}

	// The copy lands outside every server's quota, on the disk the other
	// servers on this path write to, and region data barely compresses: so it
	// is bounded by the free space the same way every other large write is,
	// and held against the other writes while it is being made.
	// Reported while it runs: Core's stage wait restarts only on a changed
	// byte count, and a tree of millions of files took longer to measure.
	var measured atomic.Int64
	stopMeasure := reportMigrationProgress(ctx, rdb, nodeToken, progressID, "staging", measured.Load)
	need := migrateOutMeasure(srcDir, &measured)
	stopMeasure()
	if budget := restoreDiskBudget(storagePath) - inflightWrites.Load(); need > budget {
		log.Printf("migrate_out %s: %d bytes to stage, %d bytes of free space to give", serverUUID, need, budget)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", fmt.Sprintf(
			"not enough free space on the source node to stage the move: need %s, can use %s", formatBytes(uint64(need)), formatBytes(uint64(max(budget, 0)))))
		return
	}
	inflightWrites.Add(need)
	defer inflightWrites.Add(-need)

	stagingDir := filepath.Join(storagePath, migrationStagingDir)
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		log.Printf("migrate_out %s: cannot create staging dir: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "cannot create staging dir")
		return
	}

	// Stamp the staging dir with our identity BEFORE archiving. This is the
	// target's only signal that it is looking at our storage rather than its
	// own, so a move we cannot prove safe must not start at all.
	if err := os.WriteFile(migrationOriginPath(storagePath, serverUUID), []byte(nodeID), 0644); err != nil {
		log.Printf("migrate_out %s: cannot write origin stamp: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "cannot write origin stamp")
		return
	}

	// Written under a name of this run's own and renamed into place only once
	// it is still wanted: the served path is the server's alone, and a
	// cancelled attempt finishing late removed the retry's archive by it.
	destZip := stagedArchivePath(storagePath, serverUUID)
	part, err := os.CreateTemp(stagingDir, serverUUID+".*.part")
	if err != nil {
		log.Printf("migrate_out %s: cannot create the archive: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "cannot create the archive")
		return
	}
	partZip := part.Name()
	part.Close()
	defer os.Remove(partZip) // gone once renamed; otherwise useless
	log.Printf("migrate_out %s: archiving %s → %s", serverUUID, srcDir, destZip)
	stopProgress := reportStagingProgress(ctx, rdb, nodeToken, progressID, partZip)
	sha, size, err := migrateOutArchive(srcDir, partZip)
	stopProgress()
	if err != nil {
		log.Printf("migrate_out %s: archive failed: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", fmt.Sprintf("archive failed: %v", err))
		return
	}
	if writesSince() {
		log.Printf("migrate_out %s: files were written while staging, dropping the archive", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "files were written to the server while the move was staging; try the move again")
		return
	}
	// Core gave up on this attempt while it was archiving: its abort found
	// nothing to remove, and the copy would sit outside the quota and keep
	// the reconciler off the server Core has started again.
	if migrateAbortCount(serverUUID) != aborts {
		log.Printf("migrate_out %s: the move was aborted while staging, dropping the archive", serverUUID)
		if cur, _ := migrateOutLatest.Load(serverUUID); cur == run {
			os.Remove(migrationOriginPath(storagePath, serverUUID))
		}
		return
	}
	if err := os.Rename(partZip, destZip); err != nil {
		log.Printf("migrate_out %s: cannot put the archive in place: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "cannot put the archive in place")
		return
	}

	meta := migrationMeta{SHA256: sha, Size: size, SourceNodeID: nodeID, StagedAt: time.Now().Unix()}
	metaJSON, _ := json.Marshal(meta)
	metaKey := queue.MigrationMetaKey(nodeToken, progressID)
	if err := rdb.Set(ctx, metaKey, metaJSON, migrationMetaTTL).Err(); err != nil {
		log.Printf("migrate_out %s: failed to publish meta: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "failed to publish meta")
		return
	}

	setMigrationStatus(ctx, rdb, nodeToken, progressID, "staged", "")
	if b, err := os.ReadFile(migrationAbortedPath(storagePath, serverUUID)); err == nil && string(b) != progressID {
		os.Remove(migrationAbortedPath(storagePath, serverUUID)) // an older attempt's, superseded
	}
	log.Printf("migrate_out %s: staged (%s, sha256=%s)", serverUUID, formatBytes(uint64(size)), sha)
}

// stagingProgressInterval is how often migrate_out reports the archive's size.
// Well inside Core's stage wait, which each report of growth restarts. A
// variable for tests.
var stagingProgressInterval = 20 * time.Second

// reportStagingProgress writes phase "staging" with the size zipPath has grown
// to, now and every stagingProgressInterval, until the returned func is called.
// That func returns only once no further write can happen, so the caller's
// final phase is never overwritten by a late "staging".
func reportStagingProgress(ctx context.Context, rdb *redis.Client, nodeToken, progressID, zipPath string) func() {
	return reportMigrationProgress(ctx, rdb, nodeToken, progressID, "staging", func() int64 {
		if fi, err := os.Stat(zipPath); err == nil {
			return fi.Size()
		}
		return 0
	})
}

// reportMigrationProgress is reportStagingProgress for any phase, with the
// byte count read from bytes.
func reportMigrationProgress(ctx context.Context, rdb *redis.Client, nodeToken, progressID, phase string, bytes func() int64) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(stagingProgressInterval)
		defer t.Stop()
		for {
			writeMigrationStatus(ctx, rdb, nodeToken, progressID, migrationStatus{Phase: phase, Bytes: bytes()})
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// handleMigrateIn (target side) pulls the staged archive from the source node
// and extracts it into a locally-selected storage path. It does NOT allocate a
// host port or start the container: Core sends the config afterwards, and the
// reconciler creates the container from it.
// migrationProbeTimeout bounds each LAN-candidate reachability probe so a
// same-LAN move stays within a few seconds before falling back to the overlay.
const migrationProbeTimeout = 2 * time.Second

// migrationProbeClient follows no redirect, for the reason migration.Pull does
// not: the candidates are the source's self-report, and the RFC1918 filter
// below means nothing if a candidate may answer with a 302 to anywhere else.
var migrationProbeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func handleMigrateIn(ctx context.Context, rdb *redis.Client, storage *StorageManager, nodeToken, serverUUID, progressID, sourceNodeID, token, expectedSha256 string, expectedSize int64, sourcePrivateIPs []string) {
	if sourceNodeID == "" || token == "" || expectedSha256 == "" {
		log.Printf("migrate_in %s: missing sourceNodeID/token/expectedSha256", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "missing migrate_in parameters")
		return
	}

	// One transfer into this server at a time. An attempt Core gave up on keeps
	// running here, and a second one extracting into the same directory, or the
	// first one's failure removing it, wrecked whatever the other produced.
	if _, busy := migrateInFlight.LoadOrStore(serverUUID, struct{}{}); busy {
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "an earlier transfer of this server is still running on the target")
		return
	}
	defer migrateInFlight.Delete(serverUUID)
	if refuseDiscardedAttempt(ctx, rdb, storage, nodeToken, serverUUID, progressID) {
		return
	}

	// Resolve where to pull from. The source node refreshes this key while it
	// is up; if it is absent the source is unreachable/down and we cannot move.
	endpointKey := fmt.Sprintf("dylaris:migration:endpoint:%s", sourceNodeID)
	endpoint, err := rdb.Get(ctx, endpointKey).Result()
	if err != nil || endpoint == "" {
		log.Printf("migrate_in %s: source endpoint %s not found: %v", serverUUID, endpointKey, err)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "source endpoint unavailable")
		return
	}

	// Choose where to pull from BEFORE allocating any storage, so a cross-LAN
	// BYON bail leaves nothing behind. Platform moves (no LAN IPs) pull over the
	// overlay endpoint, no probe — exactly today's behavior. For BYON moves we
	// probe the source's LAN IPs; reachable means a same-LAN move that pulls
	// directly over the LAN. If the LAN is unreachable (cross-LAN) we deliberately
	// do NOT hairpin the warp overlay — we report "need_remote" so the
	// orchestrator falls back to the node-direct R2 transfer path instead.
	if !pullableEndpoint(endpoint) {
		log.Printf("migrate_in %s: refusing source endpoint %q", serverUUID, endpoint)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "source endpoint is not a usable address")
		return
	}
	chosen := endpoint
	if len(sourcePrivateIPs) > 0 {
		picked := chooseMigrationHost(ctx, lanCandidates(endpoint, sourcePrivateIPs), token)
		if picked == "" {
			log.Printf("migrate_in %s: source LAN unreachable, requesting R2 fallback", serverUUID)
			setMigrationStatus(ctx, rdb, nodeToken, progressID, "need_remote", "")
			return
		}
		chosen = picked
	}

	if refuseIfStorageShared(ctx, rdb, storage, nodeToken, serverUUID, progressID, "migrate_in") {
		return
	}

	// Pick a target storage path by free space; this also creates <path>/<uuid>/
	// and persists the storage Redis key, exactly like create/migrate_storage.
	targetPath, err := storage.SelectStoragePath(serverUUID, "")
	if err != nil {
		log.Printf("migrate_in %s: cannot select storage path: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", fmt.Sprintf("storage selection failed: %v", err))
		return
	}

	markMovedIn(targetPath, serverUUID, progressID)

	url := fmt.Sprintf("http://%s/migration", chosen)
	log.Printf("migrate_in %s: pulling from %s", serverUUID, url)
	// The pull's detail stays in the log. The address came from the source
	// node, and the error text (refused, timed out, a status code, the first
	// bytes of a non-HTTP reply) reaches the server's owner through the move
	// status: a customer's node could map what this node can reach.
	// SelectStoragePath already persisted node:<nodeID>:server:<uuid>:storage,
	// so the node knows where the server lives for the follow-up start.
	receiveMovedServer(ctx, rdb, nodeToken, serverUUID, progressID, targetPath, expectedSize, "migrate_in", "pull from the source node failed",
		func(dest string, maxBytes int64, onWrite func(int64)) error {
			return migration.Pull(ctx, url, token, expectedSha256, dest, 3, maxBytes, onWrite)
		})
}

// receiveMovedServer is the target's half of a move over either transport:
// download the archive with pull, unpack it onto an empty server directory,
// and report "transferred" - or "error", with pullFailed when the download
// fails, so the transport's detail stays in the log. A hash mismatch after
// the retries fails the pull, so unverified bytes never reach the directory.
//
// Both halves are large writes on the storage path every server here shares,
// so they budget against the free space the other writes running here have not
// already claimed, and claim their own while they run: the pull its cap, the
// unpack what it has written (as backup_restore does). Both report their
// bytes, which is what keeps Core waiting on a long transfer.
func receiveMovedServer(ctx context.Context, rdb *redis.Client, nodeToken, serverUUID, progressID, targetPath string, expectedSize int64, cmd, pullFailed string, pull func(dest string, maxBytes int64, onWrite func(int64)) error) {
	var moved atomic.Int64
	stopProgress := reportMigrationProgress(ctx, rdb, nodeToken, progressID, "transferring", moved.Load)
	phase, msg := "transferred", ""
	defer func() {
		stopProgress()
		setMigrationStatus(ctx, rdb, nodeToken, progressID, phase, msg)
	}()
	fail := func(m string) { phase, msg = "error", m }

	// The size is known before the first byte: an archive that cannot fit was
	// downloaded up to the free space four times over (the retries), holding
	// that space against every other write here, and then failed vaguely.
	// Twice the size: the archive stays on disk while it is unpacked, and
	// region data barely compresses, so the copy is at least as large again.
	// Halving the budget rather than doubling a size the source node reports
	// keeps a huge one from overflowing past the check.
	if budget := restoreDiskBudget(targetPath) - inflightWrites.Load(); expectedSize > 0 && expectedSize > budget/2 {
		log.Printf("%s %s: %d bytes announced, %d bytes of free space to give", cmd, serverUUID, expectedSize, budget)
		fail(fmt.Sprintf("not enough free space on the target node to receive and unpack the move: need %s, can use %s",
			formatBytes(2*uint64(expectedSize)), formatBytes(uint64(max(budget, 0)))))
		return
	}

	tmpZip := filepath.Join(targetPath, serverUUID+".migration-in.zip")
	defer os.Remove(tmpZip)
	maxBytes := pullCap(expectedSize, targetPath)
	inflightWrites.Add(maxBytes)
	err := pull(tmpZip, maxBytes, func(n int64) { moved.Add(n) })
	inflightWrites.Add(-maxBytes)
	if err != nil {
		log.Printf("%s %s: pull failed: %v", cmd, serverUUID, err)
		fail(pullFailed)
		return
	}

	targetDir := filepath.Join(targetPath, serverUUID)
	// Onto an empty directory. A copy left here by an earlier move merged with
	// the new one: files deleted on the source since came back. Core never
	// sends this to the node the server currently lives on.
	if err := resetMigrationTarget(targetDir); err != nil {
		fail(fmt.Sprintf("cannot clear the target directory: %v", err))
		return
	}
	var written int64
	defer func() { inflightWrites.Add(-written) }()
	if err := extractMovedServer(tmpZip, targetPath, targetDir, func(n int64) {
		moved.Add(n)
		inflightWrites.Add(n)
		written += n
	}); err != nil {
		log.Printf("%s %s: extract failed: %v", cmd, serverUUID, err)
		os.RemoveAll(targetDir)
		fail(fmt.Sprintf("extract failed: %v", err))
		return
	}
	log.Printf("%s %s: transferred into %s", cmd, serverUUID, targetDir)
}

// pullCap bounds a download by the free space as well as the announced size.
// The size is the source node's own report (it writes the migration meta), so
// on its own it bounded nothing a customer's node did not want bounded, and
// the temp file lands on the storage path every server here shares. 0 from an
// older Core means "not announced", never "no bound".
func pullCap(announced int64, storagePath string) int64 {
	budget := restoreDiskBudget(storagePath) - inflightWrites.Load()
	if budget < 1 {
		budget = 1 // no room at all: any byte overflows rather than 0 = unbounded
	}
	if announced > 0 && announced < budget {
		return announced
	}
	return budget
}

// pullableEndpoint refuses an address the target must never fetch from. The
// endpoint key is written by the SOURCE node, and a customer's node can write
// anything there: this node's loopback, a link-local metadata service, or a
// name to resolve. An honest node publishes its overlay IPv4 and a port.
func pullableEndpoint(endpoint string) bool {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return false
	}
	// The value is spliced into a URL, so anything but a port number would
	// change its path.
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() &&
		!ip.IsUnspecified() && !ip.IsMulticast()
}

// extractMovedServer unpacks a moved server and drops the node files it
// arrived with. Those were written by the source node, and a customer's node
// writes what it likes: the reconciler built the container from the archived
// .node_config.json, so its image, memory, CPU and command ran on this host.
// Core sends its own config after the cutover (migrationTargetSince).
// .active_server is not read to start anything and the backups are the
// tenant's, so both stay.
func extractMovedServer(zipPath, storagePath, targetDir string, onWrite func(int64)) error {
	if err := migration.Extract(zipPath, targetDir, restoreDiskBudget(storagePath)-inflightWrites.Load(), onWrite); err != nil {
		return err
	}
	for _, name := range []string{".node_config.json", ".dylaris.json"} {
		if err := os.RemoveAll(filepath.Join(targetDir, name)); err != nil {
			return fmt.Errorf("drop the archived %s: %w", name, err)
		}
	}
	return nil
}

// reportUnstartable says "stopped" for a server a refused reconfigure leaves
// with no saved config, which is a moved server: nothing on this node can start
// it, and Core would show it "starting" for good.
func reportUnstartable(ctx context.Context, rdb *redis.Client, storage *StorageManager, uuid string) {
	if _, err := os.Stat(filepath.Join(storage.GetServerDir(uuid), ".node_config.json")); os.IsNotExist(err) {
		rdb.Set(ctx, fmt.Sprintf("dylaris:server:%s:status", uuid), "stopped", 30*time.Second)
	}
}

// lanCandidates returns the source's LAN host:port endpoints to probe, deduped.
// The overlay endpoint is deliberately NOT included: a BYON move either pulls
// over the LAN or falls back to R2, never hairpins the warp overlay. The LAN IPs
// reuse the overlay endpoint's port (the migration server listens on all
// interfaces on a single port).
// It filters: the list is the SOURCE node's self-report. It travels from that
// node's own discovery heartbeat through Core, which stores it verbatim, into
// this command - and chooseMigrationHost then sends the pull token to every
// entry. Unfiltered, a node could name this node's loopback, a metadata
// endpoint, or any host at all and have a peer probe it with a credential
// attached. isRFC1918 is the same rule privateIPv4s publishes by, so an honest
// node loses nothing.
//
// maxLANCandidates bounds the other half of the same input: each probe costs up
// to migrationProbeTimeout, sequentially, before the R2 fallback is even
// considered, so a long list is a stall on every move to this node.
func lanCandidates(overlayEndpoint string, privateIPs []string) []string {
	port := "25522"
	if _, p, err := net.SplitHostPort(overlayEndpoint); err == nil && p != "" {
		port = p
	}
	seen := map[string]bool{}
	var out []string
	for _, ip := range privateIPs {
		if !isRFC1918(net.ParseIP(strings.TrimSpace(ip))) {
			log.Printf("migrate_in: ignoring source LAN candidate %q, not a private IPv4 address", ip)
			continue
		}
		hp := net.JoinHostPort(strings.TrimSpace(ip), port)
		if seen[hp] {
			continue
		}
		seen[hp] = true
		out = append(out, hp)
		if len(out) == maxLANCandidates {
			break
		}
	}
	return out
}

// maxLANCandidates caps how many of the source's advertised LAN addresses are
// probed. A real machine has a handful of interfaces.
const maxLANCandidates = 8

// chooseMigrationHost probes each candidate with a short HEAD request (bearer
// token) and returns the first that answers 200. Empty when none responded
// within the per-candidate budget; the caller then uses the overlay endpoint.
func chooseMigrationHost(ctx context.Context, candidates []string, token string) string {
	for _, hp := range candidates {
		url := fmt.Sprintf("http://%s/migration", hp)
		cctx, cancel := context.WithTimeout(ctx, migrationProbeTimeout)
		req, err := http.NewRequestWithContext(cctx, http.MethodHead, url, nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := migrationProbeClient.Do(req)
		cancel()
		if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return hp
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
	return ""
}

// migrateInFlight holds the servers a transfer is landing into on this node.
var migrateInFlight sync.Map

// resetMigrationTarget empties a server's directory on the TARGET before an
// arriving copy is extracted into it.
func resetMigrationTarget(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

// migrationInFlight reports whether this node staged a move of the server that
// has not finished either way: the staged archive exists until the move's
// cleanup or abort removes it.
//
// Only a RECENT archive counts. One can outlive its move - every attempt
// before moves worked left one, and an abort can be lost - and while it
// counted, the server was never restarted after a crash again, silently.
// stagedArchiveMaxAge is longer than any move runs.
func migrationInFlight(storage *StorageManager, serverUUID string) bool {
	p := storage.GetServerPath(serverUUID)
	if p == "" {
		return false
	}
	st, err := os.Stat(stagedArchivePath(p, serverUUID))
	return err == nil && time.Since(st.ModTime()) < stagedArchiveMaxAge
}

// stagedArchiveMaxAge outlives the longest wait between two uses of the
// archive, each extended by progress up to Core's migrationProgressMaxWait
// (6h): migrate_in's, or on the R2 path the push's and then the pull's. The
// push dates the archive at both ends (touchStagedArchive), so no one age has
// to cover all of them.
const stagedArchiveMaxAge = 7 * time.Hour

// sweepStaleMigrationArchives removes staged archives and origin stamps older
// than any move: copies of servers left by moves that never finished, each a
// full copy outside its server's quota.
func sweepStaleMigrationArchives(storage *StorageManager) {
	for _, root := range storage.Paths() {
		dir := filepath.Join(root, migrationStagingDir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		shared := sharedWithAnotherNode(root)
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || e.IsDir() {
				continue
			}
			// A half-written archive is no move's once its writer is gone: one
			// from before this process started held its space against the
			// redelivered migrate_out, which then refused for want of room.
			// Unless another node stages here too.
			deadPart := !shared && strings.HasSuffix(e.Name(), ".part") && info.ModTime().Before(nodeStartedAt)
			if !deadPart && time.Since(info.ModTime()) < stagedArchiveMaxAge {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if err := os.Remove(p); err == nil {
				log.Printf("migration: removed stale staged file %s", p)
			}
		}
	}
}

// handleMigrateAbort (source side) removes the staged archive and origin stamp
// of a move that did not happen - and nothing else: the server stays on this
// node. The archive is a full copy of the server outside its disk quota, and
// only the successful path's cleanup used to remove it.
func handleMigrateAbort(storage *StorageManager, serverUUID string, issuedAt int64) {
	c, _ := migrateAborts.LoadOrStore(serverUUID, new(atomic.Int64))
	storagePath := storage.GetServerPath(serverUUID)
	// Recorded before the count moves: a run that reads its baseline after
	// the increment then reads this file, and one that read it before sees
	// the count change.
	if storagePath != "" && issuedAt != 0 {
		os.MkdirAll(filepath.Join(storagePath, migrationStagingDir), 0755)
		if err := os.WriteFile(migrationAbortIssuedPath(storagePath, serverUUID), []byte(strconv.FormatInt(issuedAt, 10)), 0644); err != nil {
			log.Printf("migrate_abort %s: could not record the abort: %v", serverUUID, err)
		}
	}
	c.(*atomic.Int64).Add(1)
	if storagePath == "" {
		return
	}
	if cur, ok := migrateOutLatest.Load(serverUUID); ok {
		// The run may not have made the staging dir yet.
		os.MkdirAll(filepath.Join(storagePath, migrationStagingDir), 0755)
		if err := os.WriteFile(migrationAbortedPath(storagePath, serverUUID), []byte(*cur.(*string)), 0644); err != nil {
			log.Printf("migrate_abort %s: could not record the abort: %v", serverUUID, err)
		}
	}
	for _, p := range []string{stagedArchivePath(storagePath, serverUUID), migrationOriginPath(storagePath, serverUUID)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("migrate_abort %s: could not remove %s: %v", serverUUID, p, err)
		}
	}
}

// migrateAborts counts the aborts this node has run per server (uuid ->
// *atomic.Int64). The abort carries no attempt, so a migrate_out tells one
// that arrived while it ran by the count changing.
var migrateAborts sync.Map

// migrateOutLatest holds the newest migrate_out run per server (uuid -> its
// token, a *string holding its progress id).
var migrateOutLatest sync.Map

func migrateAbortCount(uuid string) int64 {
	if c, ok := migrateAborts.Load(uuid); ok {
		return c.(*atomic.Int64).Load()
	}
	return 0
}

// migrateOutMeasure is stagedSize; a variable for tests.
var migrateOutMeasure = stagedSize

// stagedSize sums the file sizes under dir, adding each to counted as the walk
// goes so the count can be reported before the total is known (du gives only
// the total, at the end).
func stagedSize(dir string, counted *atomic.Int64) int64 {
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				counted.Add(info.Size())
			}
		}
		return nil
	})
	return counted.Load()
}

// migrateOutArchive is migration.Archive; a variable for tests.
var migrateOutArchive = migration.Archive

// migrateOutMaxAge mirrors Core's migrationStageTimeout: a migrate_out still
// unstarted this long after Core queued it is one Core has given up on and
// aborted - the abort is sent no sooner than that. Staging it anyway left a
// full copy of a server Core had started again.
const migrateOutMaxAge = 5 * time.Minute

// staleMigrateOut reports such a migrate_out. 0 is a Core that stamps none.
func staleMigrateOut(issuedAt int64, now time.Time) bool {
	return issuedAt != 0 && now.Sub(time.Unix(issuedAt, 0)) > migrateOutMaxAge
}

// dropStaleMigrateOut reports a stale migrate_out and fails its attempt: one
// redelivered after a node restart may still have Core waiting on it, which
// otherwise sat out the rest of its stage deadline. Nobody reads it when Core
// has given up.
//
// Not one this node was staging a moment ago: a restart mid-staging redelivers
// the command with Core's original stamp, and Core is still waiting on it,
// since its wait restarts on every changed report. The status key's remaining
// TTL dates the last report without either clock.
func dropStaleMigrateOut(ctx context.Context, rdb *redis.Client, nodeToken, progressID string, issuedAt int64, now time.Time) bool {
	if !staleMigrateOut(issuedAt, now) || stagedRecently(ctx, rdb, nodeToken, progressID) {
		return false
	}
	setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "the move was queued too long ago to stage; try it again")
	return true
}

// stagedRecently reports whether this node wrote phase "staging" for the
// attempt within migrateOutMaxAge.
func stagedRecently(ctx context.Context, rdb *redis.Client, nodeToken, progressID string) bool {
	key := queue.MigrationStatusKey(nodeToken, progressID)
	raw, err := rdb.Get(ctx, key).Result()
	if err != nil {
		return false
	}
	var st migrationStatus
	if json.Unmarshal([]byte(raw), &st) != nil || st.Phase != "staging" {
		return false
	}
	ttl, err := rdb.TTL(ctx, key).Result()
	return err == nil && ttl > 0 && migrationStatusTTL-ttl < migrateOutMaxAge
}

// movedInMarker sits beside a server directory a move brought here, and names
// the attempt that brought it.
func movedInMarker(storagePath, serverUUID string) string {
	return filepath.Join(storagePath, serverUUID+".moved-in")
}

// markMovedIn records that this attempt is writing the directory, so a discard
// of exactly this attempt removes it and a discard of any other - an earlier
// attempt arriving late, or a move that never reached this node - does not.
func markMovedIn(storagePath, serverUUID, progressID string) {
	if err := os.WriteFile(movedInMarker(storagePath, serverUUID), []byte(progressID), 0o600); err != nil {
		log.Printf("migrate %s: could not mark the copy: %v", serverUUID, err)
	}
}

// migrateDiscardWait bounds how long a discard waits for a pull still running.
// Past Core's longest transfer wait (6h, its migrationProgressMaxWait): a
// shorter one gave up on a pull that then finished and kept its copy for good.
var migrateDiscardWait = stagedArchiveMaxAge

// handleMigrateDiscard (target side) removes the copy a move that did not
// happen left here: a pull Core gave up waiting for still finishes and
// extracts a whole server outside any quota, and nothing else would ever
// remove it - a later delete of the server goes to the source only.
//
// It waits for a pull still running, in the background so it holds no command
// slot, and keeps the directory if a container exists for the server: that
// would mean the server does live here, whatever Core believed.
func handleMigrateDiscard(storage *StorageManager, dm *DockerManager, serverUUID, progressID string, wait time.Duration) {
	if strings.TrimSpace(serverUUID) == "" || progressID == "" {
		return
	}
	// The command is acked once this returns, and the wait below lives only
	// in memory: a restart lost it while the unacked pull was redelivered and
	// brought its copy again. On disk it stops that pull and outlives the
	// restart (resumeMigrationDiscards).
	if p := storage.GetServerPath(serverUUID); p != "" {
		if err := os.WriteFile(migrationDiscardMarker(p, serverUUID), []byte(progressID), 0o600); err != nil {
			log.Printf("migrate_discard %s: could not record the discard: %v", serverUUID, err)
		}
	}
	discardMovedCopy(storage, dm, serverUUID, progressID, wait)
}

// migrationDiscardMarker sits beside a server directory and names the attempt
// a discard was sent for.
func migrationDiscardMarker(storagePath, serverUUID string) string {
	return filepath.Join(storagePath, serverUUID+migrationDiscardSuffix)
}

const migrationDiscardSuffix = ".discarded"

// refuseDiscardedAttempt fails a transfer of an attempt Core already
// discarded, which is what a pull redelivered after a restart is. Another
// attempt's marker is spent: this one supersedes it.
//
// Every path, not the one the server resolves to: the discard that wrote the
// marker removed the copy and its storage key, and the lookup then falls back
// to the first path, which on a multi-path node need not be the marker's.
func refuseDiscardedAttempt(ctx context.Context, rdb *redis.Client, storage *StorageManager, nodeToken, serverUUID, progressID string) bool {
	if !validate.IsServerUUID(serverUUID) {
		return false
	}
	discarded := false
	for _, p := range storage.Paths() {
		m := migrationDiscardMarker(p, serverUUID)
		got, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		if string(got) == progressID {
			discarded = true
		} else {
			os.Remove(m)
		}
	}
	if !discarded {
		return false
	}
	log.Printf("migrate %s: attempt %s was discarded, not transferring it again", serverUUID, progressID)
	setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "this move was cancelled")
	return true
}

// resumeMigrationDiscards runs again the discards a restart cut off. One older
// than any move is dropped: no pull of its attempt can still succeed.
func resumeMigrationDiscards(storage *StorageManager, dm *DockerManager) {
	for _, root := range storage.Paths() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			uuid, ok := strings.CutSuffix(e.Name(), migrationDiscardSuffix)
			if !ok || e.IsDir() {
				continue
			}
			p := filepath.Join(root, e.Name())
			info, ierr := e.Info()
			got, rerr := os.ReadFile(p)
			if ierr != nil || rerr != nil {
				continue
			}
			if time.Since(info.ModTime()) >= stagedArchiveMaxAge {
				os.Remove(p)
				continue
			}
			discardMovedCopy(storage, dm, uuid, string(got), migrateDiscardWait)
		}
	}
}

// discardMovedCopy is handleMigrateDiscard's removal, in the background.
func discardMovedCopy(storage *StorageManager, dm *DockerManager, serverUUID, progressID string, wait time.Duration) {
	// Read before the goroutine: nodeSecretDir is a package variable, and the
	// background check must not race whoever sets it.
	ids := nodeIdentities(nodeSecretDir)
	go func() {
		deadline := time.Now().Add(wait)
		for {
			// Taken, not just looked at: no pull may start writing the
			// directory while this decides about it and removes it.
			if _, busy := migrateInFlight.LoadOrStore(serverUUID, struct{}{}); !busy {
				break
			}
			if time.Now().After(deadline) {
				log.Printf("migrate_discard %s: a pull is still running after %s, keeping its copy", serverUUID, wait)
				return
			}
			time.Sleep(5 * time.Second)
		}
		defer migrateInFlight.Delete(serverUUID)
		if dm != nil {
			// Only a container of THIS node's: on a shared Docker daemon the
			// source's mc_<uuid> is always there before cutover.
			if info, err := dm.cli.ContainerInspect(dm.ctx, "mc_"+serverUUID); err == nil && (info.Config == nil || ownsContainer(info.Config.Labels, ids)) {
				log.Printf("migrate_discard %s: a container exists for it here, keeping the directory", serverUUID)
				return
			}
		}
		// Shared storage: this node's directory IS the source's live one.
		if owner := sharedStorageOwner(storage, serverUUID); owner != "" {
			log.Printf("migrate_discard %s: storage is shared with %s, keeping the directory", serverUUID, owner)
			return
		}
		storagePath := storage.GetServerPath(serverUUID)
		if storagePath == "" {
			return
		}
		marker := movedInMarker(storagePath, serverUUID)
		if got, err := os.ReadFile(marker); err != nil || string(got) != progressID {
			return // not the copy this attempt brought
		}
		dir := filepath.Join(storagePath, serverUUID)
		log.Printf("migrate_discard %s: removing the copy of a move that did not happen (%s)", serverUUID, dir)
		os.RemoveAll(dir)
		os.Remove(marker)
		storage.RemoveServerPath(serverUUID)
		globalQuotaSet.RemoveQuota(serverUUID)
		forgetDiskLimit(context.Background(), storage.rdb, serverUUID)
	}()
}

// handleMigrateCleanup (source side) removes the staged archive and the
// original server directory after the target confirms transfer. The
// orchestrator controls ordering and only sends this once migrate_in reported
// "transferred". No host port to release in gateway mode.
func handleMigrateCleanup(ctx context.Context, rdb *redis.Client, storage *StorageManager, dm *DockerManager, nodeToken, serverUUID string) {
	storagePath := storage.GetServerPath(serverUUID)
	if storagePath == "" {
		log.Printf("migrate_cleanup %s: no storage path found", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "storage path not found")
		return
	}

	// Not while a move of this server INTO this node is landing: a cleanup
	// that waited in a busy queue would delete a server just moved back here.
	if _, busy := migrateInFlight.LoadOrStore(serverUUID, struct{}{}); busy {
		log.Printf("migrate_cleanup %s: a transfer of this server into this node is running, leaving its data", serverUUID)
		return
	}
	defer migrateInFlight.Delete(serverUUID)

	// The container first. It was only stopped for the move and stayed behind:
	// once Core set the server online for the target, this node's reconciler
	// started it again on the data about to be deleted, and it ran on for good
	// (and a later move back reused its stale image and binds).
	// A failed removal stops here: past this point the archive goes, and
	// with it the reconciler's reason to leave that container stopped.
	if dm != nil {
		if err := dm.PowerAction(serverUUID, "delete"); err != nil && !client.IsErrNotFound(err) {
			log.Printf("migrate_cleanup %s: container not removed, keeping the data: %v", serverUUID, err)
			setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", fmt.Sprintf("cleanup failed: container not removed: %v", err))
			return
		}
	}

	srcDir := filepath.Join(storagePath, serverUUID)
	if err := os.RemoveAll(srcDir); err != nil {
		log.Printf("migrate_cleanup %s: could not remove server dir %s: %v", serverUUID, srcDir, err)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", fmt.Sprintf("cleanup failed: %v", err))
		return
	}

	// The archive LAST: while it exists the reconciler leaves this server
	// alone, and removing it first let the recreate pass bring the container
	// back on a directory still being deleted.
	zipPath := stagedArchivePath(storagePath, serverUUID)
	if err := os.Remove(zipPath); err != nil && !os.IsNotExist(err) {
		log.Printf("migrate_cleanup %s: could not remove staged archive %s: %v", serverUUID, zipPath, err)
	}
	originPath := migrationOriginPath(storagePath, serverUUID)
	if err := os.Remove(originPath); err != nil && !os.IsNotExist(err) {
		log.Printf("migrate_cleanup %s: could not remove origin stamp %s: %v", serverUUID, originPath, err)
	}

	storage.RemoveServerPath(serverUUID)
	// As the delete path does: the cached limit has no TTL, and a move back
	// that carries "unlimited" (0) would fall back to this stale cap.
	globalQuotaSet.RemoveQuota(serverUUID)
	forgetDiskLimit(ctx, rdb, serverUUID)
	log.Printf("migrate_cleanup %s: source data removed", serverUUID)
}

// handleMigratePushR2 (source side) uploads the already-staged migration archive
// to a pre-signed S3/R2 PUT URL. Used as the cross-LAN BYON fallback: when the
// target cannot reach the source over the LAN it reports "need_remote" and the
// orchestrator routes the transfer through R2 (node-direct, $0 egress, no warp
// hairpin). The archive + its hash were already produced by migrate_out, so this
// only re-uses that staged zip — no re-archiving. Reports phase "pushed".
//
// partURLs, from a Core that knows multipart, carries one URL per partSize
// piece of the archive; Core completes the upload once we report "pushed".
// Without them the archive goes up as one PUT, as an older Core expects.
func handleMigratePushR2(ctx context.Context, rdb *redis.Client, storage *StorageManager, nodeToken, serverUUID, progressID, putURL string, partURLs []string, partSize int64) {
	if putURL == "" && len(partURLs) == 0 {
		log.Printf("migrate_push_r2 %s: missing presigned put url", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "missing push_r2 url")
		return
	}
	storagePath := storage.GetServerPath(serverUUID)
	if storagePath == "" {
		log.Printf("migrate_push_r2 %s: no storage path found", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "storage path not found")
		return
	}
	zipPath := stagedArchivePath(storagePath, serverUUID)
	if stat, err := os.Stat(zipPath); err != nil || stat.IsDir() {
		log.Printf("migrate_push_r2 %s: staged archive missing at %s", serverUUID, zipPath)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "staged archive missing")
		return
	}

	touchStagedArchive(storagePath, serverUUID)
	log.Printf("migrate_push_r2 %s: uploading staged archive to R2 (%d parts)", serverUUID, len(partURLs))
	var sent countingWriter
	stopProgress := reportMigrationProgress(ctx, rdb, nodeToken, progressID, "pushing", sent.Total)
	var err error
	if len(partURLs) > 0 {
		err = putFileMultipart(ctx, zipPath, partURLs, partSize, &sent)
	} else {
		err = putFilePresigned(ctx, putURL, zipPath, &sent)
	}
	stopProgress()
	if err != nil {
		log.Printf("migrate_push_r2 %s: upload failed: %v", serverUUID, err)
		// The detail stays in this log: the move status is served to the
		// server's owner and sub-users, and R2's error body is not theirs.
		msg := "upload to transfer storage failed"
		if errors.Is(err, errArchiveOverSinglePut) {
			// Permanent, unlike a network error, so it must read differently.
			msg = "the server's archive is larger than the 5 GiB one upload can carry, and this Core cannot upload in parts: update Core"
		}
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", msg)
		return
	}

	// The pull leg Core waits on next gets its own 6h, after this one's.
	touchStagedArchive(storagePath, serverUUID)
	setMigrationStatus(ctx, rdb, nodeToken, progressID, "pushed", "")
	log.Printf("migrate_push_r2 %s: uploaded to R2", serverUUID)
}

// touchStagedArchive dates the staged archive and origin stamp from now, so
// their age, which the restart guard and the boot sweep read, counts from the
// archive's last use rather than from the end of staging.
func touchStagedArchive(storagePath, serverUUID string) {
	now := time.Now()
	for _, p := range []string{stagedArchivePath(storagePath, serverUUID), migrationOriginPath(storagePath, serverUUID)} {
		if err := os.Chtimes(p, now, now); err != nil && !os.IsNotExist(err) {
			log.Printf("migrate_push_r2 %s: could not date %s: %v", serverUUID, p, err)
		}
	}
}

// handleMigratePullR2 (target side) downloads the migration archive from a
// pre-signed S3/R2 GET URL, verifies its sha256 against the source's expected
// hash, and extracts it. Mirrors handleMigrateIn exactly except the source is R2
// instead of the source node's HTTP server. As with the LAN/overlay path, the
// hash is checked BEFORE extract so a corrupted download never lands in the live
// server directory. Reports phase "transferred" — identical to migrate_in, so
// the orchestrator's cutover proceeds the same way.
func handleMigratePullR2(ctx context.Context, rdb *redis.Client, storage *StorageManager, nodeToken, serverUUID, progressID, getURL, expectedSha256 string, expectedSize int64) {
	if getURL == "" || expectedSha256 == "" {
		log.Printf("migrate_pull_r2 %s: missing getURL/expectedSha256", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "missing pull_r2 parameters")
		return
	}

	// One transfer into this server at a time. An attempt Core gave up on keeps
	// running here, and a second one extracting into the same directory, or the
	// first one's failure removing it, wrecked whatever the other produced.
	if _, busy := migrateInFlight.LoadOrStore(serverUUID, struct{}{}); busy {
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", "an earlier transfer of this server is still running on the target")
		return
	}
	defer migrateInFlight.Delete(serverUUID)
	if refuseDiscardedAttempt(ctx, rdb, storage, nodeToken, serverUUID, progressID) {
		return
	}

	// Same hazard as the LAN path: the R2 round trip does not make the source
	// and target directories any less identical on shared storage.
	if refuseIfStorageShared(ctx, rdb, storage, nodeToken, serverUUID, progressID, "migrate_pull_r2") {
		return
	}

	targetPath, err := storage.SelectStoragePath(serverUUID, "")
	if err != nil {
		log.Printf("migrate_pull_r2 %s: cannot select storage path: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, progressID, "error", fmt.Sprintf("storage selection failed: %v", err))
		return
	}

	markMovedIn(targetPath, serverUUID, progressID)

	log.Printf("migrate_pull_r2 %s: downloading from R2", serverUUID)
	receiveMovedServer(ctx, rdb, nodeToken, serverUUID, progressID, targetPath, expectedSize, "migrate_pull_r2", "download from transfer storage failed",
		func(dest string, maxBytes int64, onWrite func(int64)) error {
			return migration.PullURL(ctx, getURL, expectedSha256, dest, 3, maxBytes, onWrite)
		})
}

// presignedPutMaxSize is the S3/R2 single-PUT object limit. A variable for
// tests.
var presignedPutMaxSize int64 = 5 * 1024 * 1024 * 1024 // 5 GiB

// errArchiveOverSinglePut is putFilePresigned's refusal of an archive over
// presignedPutMaxSize.
var errArchiveOverSinglePut = errors.New("staged archive exceeds the 5 GiB single-upload limit")

// putFileMultipart uploads a local file through the part URLs Core signed, one
// per partSize piece in order, with the backup uploader's buffering and
// per-part retries. Core completes the upload, so nothing is completed here.
// What is read from the file is copied to read.
func putFileMultipart(ctx context.Context, filePath string, partURLs []string, partSize int64, read io.Writer) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open staged archive: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	size := stat.Size()
	// A count that does not match leaves parts unsigned or an upload Core
	// cannot complete, so it is refused before the first byte.
	if partSize <= 0 || int64(len(partURLs)) != (size+partSize-1)/partSize {
		return fmt.Errorf("Core signed %d parts of %d bytes for an archive of %d bytes", len(partURLs), partSize, size)
	}
	api := multipartAPI{
		partURLs: func(_ context.Context, parts []int32) (int64, map[int32]string, error) {
			out := make(map[int32]string, len(parts))
			for _, p := range parts {
				if p >= 1 && int(p) <= len(partURLs) {
					out[p] = partURLs[p-1]
				}
			}
			return partSize, out, nil
		},
		complete: func(context.Context) (int64, error) { return size, nil },
	}
	_, err = uploadMultipart(ctx, api, objectTransferClient, io.TeeReader(f, read), nil)
	return err
}

// putFilePresigned uploads a local file to a pre-signed PUT URL. The file is
// already on disk so its size is known up front (no temp re-staging like the
// backup path). The single-PUT 5 GiB cap (S3/R2 limit) is enforced clearly
// rather than silently truncating; larger transfers need multipart-presigned.
func putFilePresigned(ctx context.Context, url, filePath string, read io.Writer) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open staged archive: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	size := stat.Size()
	if size > presignedPutMaxSize {
		return fmt.Errorf("%w (%d bytes)", errArchiveOverSinglePut, size)
	}

	// Bounded the way a part PUT is (put): an endpoint that takes the body
	// and never answers held a command slot until the node restarted.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(partStallTimeout, cancel)
	defer stall.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, &stallReader{r: io.TeeReader(f, read), t: stall, d: partStallTimeout})
	if err != nil {
		return err
	}
	req.ContentLength = size
	resp, err := objectTransferClient.Do(req)
	if err != nil {
		return fmt.Errorf("presigned put: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("presigned put status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
