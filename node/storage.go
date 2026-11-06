package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"dylaris-pkg/storageplacement"

	"dylaris-pkg/validate"
	"github.com/redis/go-redis/v9"
)

// StorageManager manages multiple storage paths for MC server data.
// It handles path assignment for new servers (auto-balancing by free space),
// tracking which server lives on which path (via Redis), and resolving
// server UUIDs to their actual filesystem location.
type StorageManager struct {
	paths []string // Validated, available storage paths
	// skipped counts configured paths left out of paths (missing, unmounted,
	// read-only): servers on them exist but cannot be seen.
	skipped int
	rdb     *redis.Client
	mu      sync.RWMutex

	// Placement policy for NEW servers, published per node by Core. Zero value
	// (empty mode) behaves as auto, so a node that never receives the key keeps
	// the original free-space behaviour.
	placementMode  string
	placementOrder []string

	// moving holds a channel per server whose storage move is copying; it is
	// closed once the move is done. See fenceMove.
	movingMu sync.Mutex
	moving   map[string]chan struct{}
	// writers counts, per server, writes that outlive one lookup: SFTP handles,
	// Beam and panel uploads, restores, installs. Under movingMu with moving:
	// see holdWriter.
	writers map[string]int
	// writeGen counts, per server, every writer ever taken: a cross-node move
	// archives without a fence, so it tells a write that began and ended
	// while it read the tree by this number moving. See watchWrites.
	writeGen map[string]uint64
}

// Placement modes for new-server assignment. Aliases of the shared constants so
// Core, the node and the panel cannot drift apart on the spelling.
const (
	placementAuto   = storageplacement.ModeAuto
	placementManual = storageplacement.ModeManual
)

// StorageInfo holds disk usage information for a single storage path.
type StorageInfo struct {
	Path        string   `json:"path"`
	TotalBytes  uint64   `json:"total_bytes"`
	FreeBytes   uint64   `json:"free_bytes"`
	UsedBytes   uint64   `json:"used_bytes"`
	ServerCount int      `json:"server_count"`
	ServerUUIDs []string `json:"server_uuids"` // Top-level UUID directories on disk
	// QuotaEnforceable reports whether a per-server disk limit can actually be
	// ENFORCED on this path. Project quotas need xfs or ext4; on NFS/CIFS the
	// node accepts a limit and silently cannot apply it, so an operator must be
	// able to see the difference rather than trust a limit that does nothing.
	QuotaEnforceable bool `json:"quota_enforceable"`
	// Inodes of the filesystem; 0 total means unknown (and some filesystems,
	// btrfs among them, report none).
	InodesTotal uint64 `json:"inodes_total"`
	InodesFree  uint64 `json:"inodes_free"`
}

// NewStorageManager creates a StorageManager with the given paths.
// Invalid or unmounted paths are skipped with a warning.
// If no paths are valid, falls back to the default path.
func NewStorageManager(pathsCSV string, rdb *redis.Client) *StorageManager {
	sm := &StorageManager{rdb: rdb}

	if pathsCSV == "" {
		// Default: backward-compatible single path
		baseDir, _ := os.Getwd()
		defaultPath := filepath.Join(baseDir, "dylaris_data", "servers")
		os.MkdirAll(defaultPath, 0755)
		sm.paths = []string{defaultPath}
		log.Printf("storage: using default path: %s", defaultPath)
		return sm
	}

	// Parse and validate each path
	raw := strings.Split(pathsCSV, ",")
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		abs, err := filepath.Abs(p)
		if err != nil {
			log.Printf("storage: [warn] cannot resolve path %q: %v — skipping", p, err)
			sm.skipped++
			continue
		}

		// Check if path exists and is writable
		if err := validateStoragePath(abs); err != nil {
			log.Printf("storage: [warn] %s — skipping", err)
			sm.skipped++
			continue
		}

		sm.paths = append(sm.paths, abs)
		log.Printf("storage: [ok] %s", abs)
	}

	// Fallback if no paths are valid
	if len(sm.paths) == 0 {
		baseDir, _ := os.Getwd()
		defaultPath := filepath.Join(baseDir, "dylaris_data", "servers")
		os.MkdirAll(defaultPath, 0755)
		sm.paths = []string{defaultPath}
		log.Printf("storage: [warn] no configured paths valid, falling back to %s", defaultPath)
	}

	return sm
}

// validateStoragePath checks that a path exists, is a directory, and is writable.
func validateStoragePath(path string) error {
	stat, err := os.Stat(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("%s does not exist", path)
	}
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	if !stat.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}

	// Write test
	testFile := filepath.Join(path, ".dylaris_write_test")
	if err := os.WriteFile(testFile, []byte("ok"), 0644); err != nil {
		return fmt.Errorf("%s is not writable: %v", path, err)
	}
	os.Remove(testFile)

	return nil
}

// Paths returns the list of available storage paths.
func (sm *StorageManager) Paths() []string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.paths
}

// GetServerPath resolves the storage path for a server UUID.
// 1. Redis lookup: node:{nodeID}:server:{uuid}:storage
// 2. Fallback: scan all paths for the UUID directory
// 3. If not found, returns the first path (for new servers, use SelectStoragePath instead)
func (sm *StorageManager) GetServerPath(serverUUID string) string {
	// An empty UUID is not a server, and resolving it is dangerous rather than
	// merely wrong: the Redis miss falls through to the filesystem scan below,
	// where filepath.Join(p, "") is p itself. Every storage path therefore
	// "contains" it, so the resolver hands back the storage ROOT - which
	// GetServerDir returns verbatim, and which the delete command feeds to
	// os.RemoveAll. That is every server on the node, of every tenant.
	//
	// "" is the answer the callers that check already expect (handleMigrateOut
	// and handleMigrateCleanup both abort on it); it was simply never produced.
	//
	// Not just empty: "." resolved to the storage root as well (filepath.Join
	// cleans it away and the root IS a directory), ".." to its parent, and the
	// root holds .node_secret. Anything that is not a server id is no server.
	if !validate.IsServerUUID(serverUUID) {
		return ""
	}
	if ch := sm.moveInProgress(serverUUID); ch != nil {
		<-ch
	}

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// 1. Redis lookup
	keyed := false
	if sm.rdb != nil {
		key := fmt.Sprintf("node:%s:server:%s:storage", nodeID, serverUUID)
		if val, err := sm.rdb.Get(context.Background(), key).Result(); err == nil && val != "" {
			// Verify the path is still in our list
			for _, p := range sm.paths {
				if p == val {
					return val
				}
			}
			keyed = true
			log.Printf("storage: Redis path %q for %s not in active paths, scanning...", val, serverUUID)
		}
	}

	// 2. Scan all paths for existing UUID directory
	found, copies := "", 0
	for _, p := range sm.paths {
		serverDir := filepath.Join(p, serverUUID)
		if stat, err := os.Stat(serverDir); err == nil && stat.IsDir() {
			if found == "" {
				found = p
			}
			copies++
		}
	}
	if found != "" {
		// Written back only when it cannot be a guess: a key naming a path
		// that is merely not mounted yet, or a second copy a move left, made
		// the first match the server's storage for good - and the next move
		// to the live path cleared the live tree as the "leftover" (K35).
		if !keyed && copies == 1 {
			sm.setRedisPath(serverUUID, found)
		}
		return found
	}

	// 3. Not found — return first path as default
	if len(sm.paths) > 0 {
		return sm.paths[0]
	}
	return ""
}

// GetServerDir returns the full directory path for a server UUID.
// This is the convenience method: storagePath + "/" + uuid
func (sm *StorageManager) GetServerDir(serverUUID string) string {
	base := sm.GetServerPath(serverUUID)
	if base == "" {
		// Never Join onto an empty base: that yields a path relative to the
		// node's working directory, which is not this server's directory and
		// is not anything a caller may write to or delete.
		return ""
	}
	return filepath.Join(base, serverUUID)
}

// SetPlacement applies the per-node placement policy Core published. An
// unrecognised mode falls back to auto rather than leaving the node unable to
// place servers at all.
func (sm *StorageManager) SetPlacement(mode string, order []string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.placementMode = storageplacement.NormalizeMode(mode)
	sm.placementOrder = append([]string(nil), order...)
}

// orderedPaths returns this node's paths in the admin's priority order.
//
// Paths the order does not mention come LAST, in their configured order: that is
// what makes a disk added later land at the bottom without the admin having to
// edit anything, and it means an order referring to a path this node no longer
// has simply drops out. Caller must hold sm.mu.
func (sm *StorageManager) orderedPaths() []string {
	return storageplacement.OrderPaths(sm.paths, sm.placementOrder)
}

// pickByPriority returns the first usable path in the admin's order, or "" when
// none is usable.
//
// "Usable" only means the path currently reports free space: a path that is
// unmounted or full reports 0 and is skipped. Manual placement deliberately does
// NOT balance beyond that - the admin chose the order, and second-guessing it
// with a free-space heuristic would make the setting mean nothing.
// Caller must hold sm.mu.
func (sm *StorageManager) pickByPriority() string {
	for _, p := range sm.orderedPaths() {
		if getFreeSpace(p) > 0 {
			return p
		}
	}
	return ""
}

// SelectStoragePath picks the path for a NEW server: an explicit request wins,
// then the admin's manual order if configured, then most free space.
// After selection, it creates the server directory and stores the assignment in Redis.
func (sm *StorageManager) SelectStoragePath(serverUUID string, preferredPath string) (string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// If admin explicitly chose a path, use it (if valid)
	if preferredPath != "" {
		for _, p := range sm.paths {
			if p == preferredPath {
				return sm.assignPath(serverUUID, p)
			}
		}
		return "", fmt.Errorf("preferred path %q not available", preferredPath)
	}

	if sm.placementMode == placementManual {
		if p := sm.pickByPriority(); p != "" {
			return sm.assignPath(serverUUID, p)
		}
		// Every path in the order is gone or full. Fall through to free-space
		// selection instead of refusing: a stale order must not take the node
		// out of service.
		log.Printf("storage: manual placement order has no usable path, falling back to free space")
	}

	// Auto: pick path with most free space
	var bestPath string
	var bestFree uint64

	for _, p := range sm.paths {
		free := getFreeSpace(p)
		if free > bestFree {
			bestFree = free
			bestPath = p
		}
	}

	if bestPath == "" {
		return "", fmt.Errorf("no storage paths available")
	}

	return sm.assignPath(serverUUID, bestPath)
}

// assignPath creates the server directory and stores the path in Redis.
// Caller must hold sm.mu.
func (sm *StorageManager) assignPath(serverUUID, storagePath string) (string, error) {
	serverDir := filepath.Join(storagePath, serverUUID)
	if err := os.MkdirAll(serverDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create server directory: %v", err)
	}

	sm.setRedisPath(serverUUID, storagePath)
	log.Printf("storage: assigned %s → %s", serverUUID, storagePath)
	return storagePath, nil
}

// RemoveServerPath cleans up the Redis key when a server is deleted.
func (sm *StorageManager) RemoveServerPath(serverUUID string) {
	if sm.rdb != nil {
		key := fmt.Sprintf("node:%s:server:%s:storage", nodeID, serverUUID)
		sm.rdb.Del(context.Background(), key)
	}
}

// GetStorageInfo returns disk usage info for all storage paths.
func (sm *StorageManager) GetStorageInfo() []StorageInfo {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var infos []StorageInfo
	for _, p := range sm.paths {
		info := StorageInfo{Path: p}

		total, free := getDiskSpace(p)
		info.TotalBytes = total
		info.FreeBytes = free
		info.InodesTotal, info.InodesFree = getInodes(p)
		if total > free {
			info.UsedBytes = total - free
		}

		// Count and list server directories (UUID folders)
		entries, err := os.ReadDir(p)
		info.ServerUUIDs = []string{}
		if err == nil {
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
					info.ServerCount++
					info.ServerUUIDs = append(info.ServerUUIDs, e.Name())
				}
			}
		}

		infos = append(infos, info)
	}
	return infos
}

// setRedisPath stores the storage path for a server in Redis (no expiry).
func (sm *StorageManager) setRedisPath(serverUUID, storagePath string) error {
	if sm.rdb == nil {
		return nil
	}
	key := fmt.Sprintf("node:%s:server:%s:storage", nodeID, serverUUID)
	if err := sm.rdb.Set(context.Background(), key, storagePath, 0).Err(); err != nil {
		log.Printf("storage: failed to set Redis key %s: %v", key, err)
		return err
	}
	return nil
}

// getFreeSpace returns the available bytes on the filesystem containing path.
func getFreeSpace(path string) uint64 {
	_, free := getDiskSpace(path)
	return free
}

// LogStorageInfo logs a summary of all storage paths at startup.
func (sm *StorageManager) LogStorageInfo() {
	infos := sm.GetStorageInfo()
	for _, info := range infos {
		totalGB := float64(info.TotalBytes) / (1024 * 1024 * 1024)
		freeGB := float64(info.FreeBytes) / (1024 * 1024 * 1024)
		log.Printf("storage: %s — %.1f GB free / %.1f GB total — %d servers",
			info.Path, freeGB, totalGB, info.ServerCount)
	}
}

// resolveMigration validates a storage move and finds the server's current
// path. Only this part needs sm.mu: the copy in MigrateServerPath can take
// minutes, and holding the write lock through it stalled the heartbeat and
// every other server's path lookup on the node. migrate_storage of one server
// is already serialised by the command in-flight guard.
func (sm *StorageManager) resolveMigration(serverUUID, targetPath string) (string, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Validate target is in our configured paths
	validTarget := false
	for _, p := range sm.paths {
		if p == targetPath {
			validTarget = true
			break
		}
	}
	if !validTarget {
		return "", fmt.Errorf("target path %q is not a configured storage path", targetPath)
	}

	// Find current path: Redis lookup first, then filesystem scan
	currentPath := ""
	if sm.rdb != nil {
		key := fmt.Sprintf("node:%s:server:%s:storage", nodeID, serverUUID)
		if val, err := sm.rdb.Get(context.Background(), key).Result(); err == nil && val != "" {
			for _, p := range sm.paths {
				if p == val {
					currentPath = val
					break
				}
			}
		}
	}
	scanned := currentPath == ""
	if scanned {
		for _, p := range sm.paths {
			if stat, err := os.Stat(filepath.Join(p, serverUUID)); err == nil && stat.IsDir() {
				currentPath = p
				break
			}
		}
	}
	if currentPath == "" {
		return "", fmt.Errorf("server %s not found on any storage path", serverUUID)
	}
	if currentPath == targetPath {
		return "", fmt.Errorf("server %s is already on path %q", serverUUID, targetPath)
	}
	// The move clears the target as a leftover, which only the storage key
	// can vouch for. Found by scanning, either copy may be the live one.
	if scanned {
		if _, err := os.Stat(filepath.Join(targetPath, serverUUID)); !os.IsNotExist(err) {
			return "", fmt.Errorf("server %s has a directory on both %q and %q and no storage key says which one is live; refusing the move", serverUUID, currentPath, targetPath)
		}
	}
	return currentPath, nil
}

// fenceMove makes every lookup of this one server wait until the returned
// func runs. During the copy the storage key still names the source, so a
// write resolved then landed in the tree the move deletes afterwards. The old
// manager-wide lock gave the same wait, for every server on the node.
//
// A lookup made before the fence is past it: an open SFTP handle or a Beam
// upload kept writing into the source, the move deleted it, and the client saw
// the upload succeed. So the fence is refused while a writer holds the server.
func (sm *StorageManager) fenceMove(serverUUID string) (func(), error) {
	ch := make(chan struct{})
	sm.movingMu.Lock()
	if n := sm.writers[serverUUID]; n > 0 {
		sm.movingMu.Unlock()
		return nil, writersInProgress(serverUUID, n)
	}
	if sm.moving == nil {
		sm.moving = map[string]chan struct{}{}
	}
	sm.moving[serverUUID] = ch
	sm.movingMu.Unlock()
	return func() {
		sm.movingMu.Lock()
		delete(sm.moving, serverUUID)
		sm.movingMu.Unlock()
		close(ch)
	}, nil
}

func writersInProgress(serverUUID string, n int) error {
	return fmt.Errorf("server %s has %d write(s) in progress (uploads, installs or a restore); retry the move once they finish", serverUUID, n)
}

// CheckMigration makes the refusals MigrateServerPath would make, moving
// nothing. The handler stops the server before the move, so a refusal that
// came only after the stop took a live server down for nothing.
func (sm *StorageManager) CheckMigration(serverUUID, targetPath string) error {
	currentPath, err := sm.resolveMigration(serverUUID, targetPath)
	if err != nil {
		return err
	}
	sm.movingMu.Lock()
	n := sm.writers[serverUUID]
	sm.movingMu.Unlock()
	if n > 0 {
		return writersInProgress(serverUUID, n)
	}
	// The move clears a leftover copy at the target before its own check.
	dstDir := filepath.Join(targetPath, serverUUID)
	_, err = checkMigrationSpace(filepath.Join(currentPath, serverUUID), targetPath, uint64(dirSize(dstDir)))
	return err
}

// checkMigrationSpace returns the size of srcDir, or an error when targetPath
// cannot hold it. reclaim is what the move frees on the target before copying.
func checkMigrationSpace(srcDir, targetPath string, reclaim uint64) (int64, error) {
	srcSize := dirSize(srcDir) // int64, from stats_collector.go
	const spaceBuffer = uint64(512 * 1024 * 1024)
	free := getFreeSpace(targetPath) + reclaim
	if srcSize > 0 && free < uint64(srcSize)+spaceBuffer {
		return 0, fmt.Errorf("not enough free space on %s: need %s (+512MB buffer), have %s free",
			targetPath, formatBytes(uint64(srcSize)), formatBytes(free))
	}
	return srcSize, nil
}

// holdWriter registers a write handle for the server until the returned func
// runs. Taken BEFORE the handle resolves the server's path: a move that fenced
// first then makes that lookup wait for the move, and one that comes later
// sees the writer and is refused.
func (sm *StorageManager) holdWriter(serverUUID string) func() {
	unhold, _ := sm.holdWriterUnlessMoving(serverUUID, false)
	return unhold
}

// tryHoldWriter is holdWriter for a caller that must not wait for a move: the
// mesh read loop, which reads nothing else from that Core while it waits. It
// returns ok=false, holding nothing, when a move of the server is running.
// The check and the hold are one step under movingMu, so a move cannot fence
// in between and make the lookup that follows wait after all.
func (sm *StorageManager) tryHoldWriter(serverUUID string) (func(), bool) {
	return sm.holdWriterUnlessMoving(serverUUID, true)
}

func (sm *StorageManager) holdWriterUnlessMoving(serverUUID string, refuseMoving bool) (func(), bool) {
	if sm == nil {
		return func() {}, true
	}
	sm.movingMu.Lock()
	if refuseMoving && sm.moving[serverUUID] != nil {
		sm.movingMu.Unlock()
		return func() {}, false
	}
	if sm.writers == nil {
		sm.writers = map[string]int{}
	}
	sm.writers[serverUUID]++
	if sm.writeGen == nil {
		sm.writeGen = map[string]uint64{}
	}
	sm.writeGen[serverUUID]++
	sm.movingMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			sm.movingMu.Lock()
			if sm.writers[serverUUID]--; sm.writers[serverUUID] <= 0 {
				delete(sm.writers, serverUUID)
			}
			sm.movingMu.Unlock()
		})
	}, true
}

// watchWrites is for a reader of the whole tree that must not make lookups
// wait, as fenceMove does: migrate_out archives for minutes, and a fence held
// that long stalled the stats and reconcile loops and a retry of the same move.
// It is refused while a writer holds the server, like a fence, and the
// returned func reports whether any write began since, so a copy that may be
// missing or tearing a write is dropped instead of becoming the server.
func (sm *StorageManager) watchWrites(serverUUID string) (func() bool, error) {
	sm.movingMu.Lock()
	defer sm.movingMu.Unlock()
	if n := sm.writers[serverUUID]; n > 0 {
		return nil, writersInProgress(serverUUID, n)
	}
	start := sm.writeGen[serverUUID]
	return func() bool {
		sm.movingMu.Lock()
		defer sm.movingMu.Unlock()
		return sm.writeGen[serverUUID] != start || sm.writers[serverUUID] > 0
	}, nil
}

// moveInProgress returns the channel a lookup of this server waits on, or nil.
func (sm *StorageManager) moveInProgress(serverUUID string) chan struct{} {
	sm.movingMu.Lock()
	defer sm.movingMu.Unlock()
	return sm.moving[serverUUID]
}

// migrateCopyTree is a var so a test can hold the copy open.
var migrateCopyTree = copyTree

// MigrateServerPath moves a server's data directory from its current storage path
// to targetPath. The server container must be stopped before calling this.
// Steps: validate → find current → clear target → check space → copy → verify → update Redis → delete old.
func (sm *StorageManager) MigrateServerPath(serverUUID, targetPath string) error {
	currentPath, err := sm.resolveMigration(serverUUID, targetPath)
	if err != nil {
		return err
	}

	release, err := sm.fenceMove(serverUUID)
	if err != nil {
		return err
	}
	defer release()

	srcDir := filepath.Join(currentPath, serverUUID)
	dstDir := filepath.Join(targetPath, serverUUID)

	// A tree already at the target is a copy an earlier move left (the storage
	// key names the source; resolveMigration refuses when it does not). Copied over,
	// it merged: files deleted from the live tree since came back. Cleared
	// before the space check, which it would otherwise count as used.
	if err := os.RemoveAll(dstDir); err != nil {
		return fmt.Errorf("clear the leftover copy at %s: %w", dstDir, err)
	}

	srcSize, err := checkMigrationSpace(srcDir, targetPath, 0)
	if err != nil {
		return err
	}

	log.Printf("storage: migrating %s: %s → %s (%s)", serverUUID, currentPath, targetPath, formatBytes(uint64(srcSize)))

	// copyTree, not copyDir: this is a move, so the destination has to be the
	// whole server including the entries copyDir withholds from a user-facing
	// duplicate. With backups present copyDir loses enough bytes to fail the
	// 90% check below; without them it passes and the source is deleted, taking
	// .active_server and .node_config.json with it.
	if err := migrateCopyTree(srcDir, dstDir); err != nil {
		os.RemoveAll(dstDir)
		return fmt.Errorf("copy failed: %v", err)
	}

	// Verify: dest must be at least 90% of source size
	if dstSize := dirSize(dstDir); srcSize > 0 && dstSize < srcSize*9/10 {
		os.RemoveAll(dstDir)
		return fmt.Errorf("copy verification failed: source=%s, dest=%s", formatBytes(uint64(srcSize)), formatBytes(uint64(dstSize)))
	}

	// The key moves before the source goes: a node killed during the delete,
	// or a key that could not be written, left the server resolving to a
	// half-deleted tree. Both copies are whole here, so on a failed write
	// neither is touched, whichever one the key ended up naming.
	if err := sm.setRedisPath(serverUUID, targetPath); err != nil {
		return fmt.Errorf("record the new storage path: %w", err)
	}

	// Remove old directory (data is safe at target)
	if err := os.RemoveAll(srcDir); err != nil {
		log.Printf("storage: [warn] could not remove old directory %s: %v", srcDir, err)
	}
	log.Printf("storage: migration complete: %s → %s", serverUUID, targetPath)
	return nil
}

// formatBytes returns a human-readable byte size string.
func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
