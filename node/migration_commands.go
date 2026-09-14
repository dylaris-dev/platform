package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dylaris-pkg/migration"
	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"
)

// Auto-move (Wave 2b) command handlers. Migration is GATEWAY-ONLY: in gateway
// mode the node binds no host ports (see docker_mgr.go), so a move only relocates
// the server's data directory. No host port is allocated or released here.
//
// Redis keys this file owns:
//   dylaris:migration:<uuid>:meta    JSON {sha256,size,sourceNodeID,stagedAt}, TTL ~1h
//   dylaris:migration:<uuid>:status  JSON {phase,error?}, TTL ~1h
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
}

// stagedArchivePath returns the staged-zip path for a server given its storage
// path: <storagePath>/.dylaris-migration/<uuid>.zip.
func stagedArchivePath(storagePath, serverUUID string) string {
	return filepath.Join(storagePath, migrationStagingDir, serverUUID+".zip")
}

// migrationOriginPath is the stamp migrate_out drops next to the staged archive,
// naming the node that staged it.
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
func refuseIfStorageShared(ctx context.Context, rdb *redis.Client, storage *StorageManager, nodeToken, serverUUID, cmd string) bool {
	owner := sharedStorageOwner(storage, serverUUID)
	if owner == "" {
		return false
	}
	log.Printf("%s %s: refusing - storage is shared with source node %s", cmd, serverUUID, owner)
	setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", fmt.Sprintf(
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
	data, err := json.Marshal(migrationStatus{Phase: phase, Error: errMsg})
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
func handleMigrateOut(ctx context.Context, rdb *redis.Client, storage *StorageManager, nodeToken, serverUUID string) {
	storagePath := storage.GetServerPath(serverUUID)
	if storagePath == "" {
		log.Printf("migrate_out %s: no storage path found", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "storage path not found")
		return
	}
	srcDir := filepath.Join(storagePath, serverUUID)
	if stat, err := os.Stat(srcDir); err != nil || !stat.IsDir() {
		log.Printf("migrate_out %s: server dir missing at %s", serverUUID, srcDir)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "server directory missing")
		return
	}

	stagingDir := filepath.Join(storagePath, migrationStagingDir)
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		log.Printf("migrate_out %s: cannot create staging dir: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "cannot create staging dir")
		return
	}

	// Stamp the staging dir with our identity BEFORE archiving. This is the
	// target's only signal that it is looking at our storage rather than its
	// own, so a move we cannot prove safe must not start at all.
	if err := os.WriteFile(migrationOriginPath(storagePath, serverUUID), []byte(nodeID), 0644); err != nil {
		log.Printf("migrate_out %s: cannot write origin stamp: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "cannot write origin stamp")
		return
	}

	destZip := stagedArchivePath(storagePath, serverUUID)
	log.Printf("migrate_out %s: archiving %s → %s", serverUUID, srcDir, destZip)
	sha, size, err := migration.Archive(srcDir, destZip)
	if err != nil {
		log.Printf("migrate_out %s: archive failed: %v", serverUUID, err)
		os.Remove(destZip) // partial archive is useless
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", fmt.Sprintf("archive failed: %v", err))
		return
	}

	meta := migrationMeta{SHA256: sha, Size: size, SourceNodeID: nodeID, StagedAt: time.Now().Unix()}
	metaJSON, _ := json.Marshal(meta)
	metaKey := queue.MigrationMetaKey(nodeToken, serverUUID)
	if err := rdb.Set(ctx, metaKey, metaJSON, migrationMetaTTL).Err(); err != nil {
		log.Printf("migrate_out %s: failed to publish meta: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "failed to publish meta")
		return
	}

	setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "staged", "")
	log.Printf("migrate_out %s: staged (%s, sha256=%s)", serverUUID, formatBytes(uint64(size)), sha)
}

// handleMigrateIn (target side) pulls the staged archive from the source node
// and extracts it into a locally-selected storage path. It does NOT allocate a
// host port or start the container — the orchestrator sends a normal start
// afterwards, which recreates the container from the extracted .node_config.json.
// migrationProbeTimeout bounds each LAN-candidate reachability probe so a
// same-LAN move stays within a few seconds before falling back to the overlay.
const migrationProbeTimeout = 2 * time.Second

func handleMigrateIn(ctx context.Context, rdb *redis.Client, storage *StorageManager, nodeToken, serverUUID, sourceNodeID, token, expectedSha256 string, expectedSize int64, sourcePrivateIPs []string) {
	if sourceNodeID == "" || token == "" || expectedSha256 == "" {
		log.Printf("migrate_in %s: missing sourceNodeID/token/expectedSha256", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "missing migrate_in parameters")
		return
	}

	// Resolve where to pull from. The source node refreshes this key while it
	// is up; if it is absent the source is unreachable/down and we cannot move.
	endpointKey := fmt.Sprintf("dylaris:migration:endpoint:%s", sourceNodeID)
	endpoint, err := rdb.Get(ctx, endpointKey).Result()
	if err != nil || endpoint == "" {
		log.Printf("migrate_in %s: source endpoint %s not found: %v", serverUUID, endpointKey, err)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "source endpoint unavailable")
		return
	}

	// Choose where to pull from BEFORE allocating any storage, so a cross-LAN
	// BYON bail leaves nothing behind. Platform moves (no LAN IPs) pull over the
	// overlay endpoint, no probe — exactly today's behavior. For BYON moves we
	// probe the source's LAN IPs; reachable means a same-LAN move that pulls
	// directly over the LAN. If the LAN is unreachable (cross-LAN) we deliberately
	// do NOT hairpin the warp overlay — we report "need_remote" so the
	// orchestrator falls back to the node-direct R2 transfer path instead.
	chosen := endpoint
	if len(sourcePrivateIPs) > 0 {
		picked := chooseMigrationHost(ctx, lanCandidates(endpoint, sourcePrivateIPs), token)
		if picked == "" {
			log.Printf("migrate_in %s: source LAN unreachable, requesting R2 fallback", serverUUID)
			setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "need_remote", "")
			return
		}
		chosen = picked
	}

	if refuseIfStorageShared(ctx, rdb, storage, nodeToken, serverUUID, "migrate_in") {
		return
	}

	// Pick a target storage path by free space; this also creates <path>/<uuid>/
	// and persists the storage Redis key, exactly like create/migrate_storage.
	targetPath, err := storage.SelectStoragePath(serverUUID, "")
	if err != nil {
		log.Printf("migrate_in %s: cannot select storage path: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", fmt.Sprintf("storage selection failed: %v", err))
		return
	}

	url := fmt.Sprintf("http://%s/migration", chosen)
	tmpZip := filepath.Join(targetPath, serverUUID+".migration-in.zip")
	log.Printf("migrate_in %s: pulling from %s", serverUUID, url)
	if err := migration.Pull(ctx, url, token, expectedSha256, tmpZip, 3, expectedSize); err != nil {
		log.Printf("migrate_in %s: pull failed: %v", serverUUID, err)
		os.Remove(tmpZip)
		// Hash mismatch after retries aborts before extract — never write
		// unverified bytes into the live server directory.
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", fmt.Sprintf("pull failed: %v", err))
		return
	}

	targetDir := filepath.Join(targetPath, serverUUID)
	if err := migration.Extract(tmpZip, targetDir); err != nil {
		log.Printf("migrate_in %s: extract failed: %v", serverUUID, err)
		os.Remove(tmpZip)
		os.RemoveAll(targetDir)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", fmt.Sprintf("extract failed: %v", err))
		return
	}
	os.Remove(tmpZip)

	// SelectStoragePath already persisted node:<nodeID>:server:<uuid>:storage,
	// so the node knows where the server lives for the follow-up start.

	setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "transferred", "")
	log.Printf("migrate_in %s: transferred into %s", serverUUID, targetDir)
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
		resp, err := http.DefaultClient.Do(req)
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

// handleMigrateCleanup (source side) removes the staged archive and the
// original server directory after the target confirms transfer. The
// orchestrator controls ordering and only sends this once migrate_in reported
// "transferred". No host port to release in gateway mode.
func handleMigrateCleanup(ctx context.Context, rdb *redis.Client, storage *StorageManager, nodeToken, serverUUID string) {
	storagePath := storage.GetServerPath(serverUUID)
	if storagePath == "" {
		log.Printf("migrate_cleanup %s: no storage path found", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "storage path not found")
		return
	}

	zipPath := stagedArchivePath(storagePath, serverUUID)
	if err := os.Remove(zipPath); err != nil && !os.IsNotExist(err) {
		log.Printf("migrate_cleanup %s: could not remove staged archive %s: %v", serverUUID, zipPath, err)
	}
	originPath := migrationOriginPath(storagePath, serverUUID)
	if err := os.Remove(originPath); err != nil && !os.IsNotExist(err) {
		log.Printf("migrate_cleanup %s: could not remove origin stamp %s: %v", serverUUID, originPath, err)
	}

	srcDir := filepath.Join(storagePath, serverUUID)
	if err := os.RemoveAll(srcDir); err != nil {
		log.Printf("migrate_cleanup %s: could not remove server dir %s: %v", serverUUID, srcDir, err)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", fmt.Sprintf("cleanup failed: %v", err))
		return
	}

	storage.RemoveServerPath(serverUUID)
	log.Printf("migrate_cleanup %s: source data removed", serverUUID)
}

// handleMigratePushR2 (source side) uploads the already-staged migration archive
// to a pre-signed S3/R2 PUT URL. Used as the cross-LAN BYON fallback: when the
// target cannot reach the source over the LAN it reports "need_remote" and the
// orchestrator routes the transfer through R2 (node-direct, $0 egress, no warp
// hairpin). The archive + its hash were already produced by migrate_out, so this
// only re-uses that staged zip — no re-archiving. Reports phase "pushed".
func handleMigratePushR2(ctx context.Context, rdb *redis.Client, storage *StorageManager, nodeToken, serverUUID, putURL string) {
	if putURL == "" {
		log.Printf("migrate_push_r2 %s: missing presigned put url", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "missing push_r2 url")
		return
	}
	storagePath := storage.GetServerPath(serverUUID)
	if storagePath == "" {
		log.Printf("migrate_push_r2 %s: no storage path found", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "storage path not found")
		return
	}
	zipPath := stagedArchivePath(storagePath, serverUUID)
	if stat, err := os.Stat(zipPath); err != nil || stat.IsDir() {
		log.Printf("migrate_push_r2 %s: staged archive missing at %s", serverUUID, zipPath)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "staged archive missing")
		return
	}

	log.Printf("migrate_push_r2 %s: uploading staged archive to R2", serverUUID)
	if err := putFilePresigned(ctx, putURL, zipPath); err != nil {
		log.Printf("migrate_push_r2 %s: upload failed: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", fmt.Sprintf("r2 push failed: %v", err))
		return
	}

	setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "pushed", "")
	log.Printf("migrate_push_r2 %s: uploaded to R2", serverUUID)
}

// handleMigratePullR2 (target side) downloads the migration archive from a
// pre-signed S3/R2 GET URL, verifies its sha256 against the source's expected
// hash, and extracts it. Mirrors handleMigrateIn exactly except the source is R2
// instead of the source node's HTTP server. As with the LAN/overlay path, the
// hash is checked BEFORE extract so a corrupted download never lands in the live
// server directory. Reports phase "transferred" — identical to migrate_in, so
// the orchestrator's cutover proceeds the same way.
func handleMigratePullR2(ctx context.Context, rdb *redis.Client, storage *StorageManager, nodeToken, serverUUID, getURL, expectedSha256 string, expectedSize int64) {
	if getURL == "" || expectedSha256 == "" {
		log.Printf("migrate_pull_r2 %s: missing getURL/expectedSha256", serverUUID)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", "missing pull_r2 parameters")
		return
	}

	// Same hazard as the LAN path: the R2 round trip does not make the source
	// and target directories any less identical on shared storage.
	if refuseIfStorageShared(ctx, rdb, storage, nodeToken, serverUUID, "migrate_pull_r2") {
		return
	}

	targetPath, err := storage.SelectStoragePath(serverUUID, "")
	if err != nil {
		log.Printf("migrate_pull_r2 %s: cannot select storage path: %v", serverUUID, err)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", fmt.Sprintf("storage selection failed: %v", err))
		return
	}

	tmpZip := filepath.Join(targetPath, serverUUID+".migration-in.zip")
	log.Printf("migrate_pull_r2 %s: downloading from R2", serverUUID)
	if err := migration.PullURL(ctx, getURL, expectedSha256, tmpZip, 3, expectedSize); err != nil {
		log.Printf("migrate_pull_r2 %s: download failed: %v", serverUUID, err)
		os.Remove(tmpZip)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", fmt.Sprintf("r2 pull failed: %v", err))
		return
	}

	targetDir := filepath.Join(targetPath, serverUUID)
	if err := migration.Extract(tmpZip, targetDir); err != nil {
		log.Printf("migrate_pull_r2 %s: extract failed: %v", serverUUID, err)
		os.Remove(tmpZip)
		os.RemoveAll(targetDir)
		setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "error", fmt.Sprintf("extract failed: %v", err))
		return
	}
	os.Remove(tmpZip)

	setMigrationStatus(ctx, rdb, nodeToken, serverUUID, "transferred", "")
	log.Printf("migrate_pull_r2 %s: transferred into %s", serverUUID, targetDir)
}

// presignedPutMaxSize is the S3/R2 single-PUT object limit.
const presignedPutMaxSize = 5 * 1024 * 1024 * 1024 // 5 GiB

// putFilePresigned uploads a local file to a pre-signed PUT URL. The file is
// already on disk so its size is known up front (no temp re-staging like the
// backup path). The single-PUT 5 GiB cap (S3/R2 limit) is enforced clearly
// rather than silently truncating; larger transfers need multipart-presigned.
func putFilePresigned(ctx context.Context, url, filePath string) error {
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
		return fmt.Errorf("staged archive %d bytes exceeds the 5 GiB single-upload limit", size)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
	if err != nil {
		return err
	}
	req.ContentLength = size
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("presigned put: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("presigned put status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
