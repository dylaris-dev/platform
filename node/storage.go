package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// skippedPaths are the configured paths among them: a key naming one is a
	// server whose storage is not available right now, not a retired path.
	skippedPaths map[string]bool
	rdb          *redis.Client
	mu           sync.RWMutex

	// Placement policy for NEW servers, published per node by Core. Zero value
	// (empty mode) behaves as auto, so a node that never receives the key keeps
	// the original free-space behaviour.
	placementMode  string
	placementOrder []string

	// moving holds a channel per server whose storage move is copying; it is
	// closed once the move is done. See fenceMove.
	movingMu sync.Mutex
	moving   map[string]chan struct{}

	// probes holds each path's last GetStorageInfo probe, under probeMu.
	probeMu sync.Mutex
	probes  map[string]*storageProbe
	// ambiguous are the servers whose copies nothing tells apart, with the
	// paths holding them, under ambMu. See StorageAmbiguities.
	ambMu     sync.Mutex
	ambiguous map[string][]string
	// writers counts, per server, writes that outlive one lookup: SFTP handles,
	// Beam and panel uploads, restores, installs. Under movingMu with moving:
	// see holdWriter.
	writers map[string]int
	// writeGen counts, per server, every writer ever taken: a cross-node move
	// archives without a fence, so it tells a write that began and ended
	// while it read the tree by this number moving. See watchWrites.
	writeGen map[string]uint64
	// readers counts, per server, folder zip downloads in progress. Apart
	// from writers so a download does not bump writeGen. See holdReader.
	readers map[string]int
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
	// unreadable: ReadDir failed, so ServerUUIDs says nothing about the path.
	unreadable bool
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
			sm.skipPath(p)
			continue
		}

		// Check if path exists and is writable
		if err := validateStoragePath(abs); err != nil {
			log.Printf("storage: [warn] %s — skipping", err)
			sm.skipPath(abs)
			continue
		}

		sm.paths = append(sm.paths, abs)
	}
	sm.paths = sm.dropUnmountedPaths(sm.paths)
	for _, p := range sm.paths {
		log.Printf("storage: [ok] %s", p)
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

// skipPath records a configured path left out of sm.paths.
func (sm *StorageManager) skipPath(p string) {
	sm.skipped++
	if sm.skippedPaths == nil {
		sm.skippedPaths = map[string]bool{}
	}
	sm.skippedPaths[p] = true
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

// storageMarkerName sits in the root of every storage path this node took. It
// lists every path the node ever marked, so each disk vouches for the others.
const storageMarkerName = ".dylaris-storage"

// storageMarkerGen prefixes the marker line that numbers the boot which wrote
// it. Every boot writes the same number onto every path it took, so a marker
// older than another one was not rewritten while its path was taken: it is
// the copy a boot wrote onto the bare mountpoint, hidden under the disk since,
// or the disk's own after a boot it was missing from. Either way it does not
// vouch for the path. Old nodes read the line as a path no one configures.
const storageMarkerGen = "#gen "

// storageMarkerKept prefixes the marker lines "<gen> <path>": the gen of the
// last boot that took each listed path. A disk missing for one boot keeps the
// older gen, and without this it read as its bare mountpoint for good once it
// returned holding no server (Z13). The bare copy carries a gen of a boot
// before the disk was last taken, so it still does not match.
const storageMarkerKept = "#kept "

// storageMarkerNode prefixes the marker line naming the node id (.node_id)
// the boot that wrote it ran as. A first path whose own identity files are to
// vouch for it must hold that id: a bare mountpoint can carry identity files
// too, written by a boot that ran on it (R24-11). Old nodes read the line as a
// path no one configures and write it back unchanged.
const storageMarkerNode = "#node "

// readStorageMarkers reads the markers on paths: every path any of them
// lists, which paths carry a current marker of their own, the last gen that
// took each path, the highest gen seen, and the node ids the current markers
// record. A path is unmounted when it is known, not marked, and holds nothing.
//
// A marker recording a node id not in ownIDs (empty = no filter) is another
// node's disk: it says nothing about this node's paths, so its path list,
// #kept lines and gen are not counted against them (R27-05/R27-06). Its own
// path is still judged by this node's gens, and maxGen still counts its gen,
// so the next gen written is above every marker on the paths.
func readStorageMarkers(paths []string, ownIDs map[string]bool) (known, marked map[string]bool, lastKept map[string]uint64, maxGen uint64, nodeIDs map[string]bool) {
	known = map[string]bool{}
	marked = map[string]bool{}
	listed := map[string]bool{}
	gens := map[string]uint64{}
	lastKept = map[string]uint64{}
	ids := map[string]string{}
	var ownMax uint64
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Join(p, storageMarkerName))
		if err != nil {
			continue
		}
		marked[p] = true
		var lines []string
		kept := map[string]uint64{}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if g, ok := strings.CutPrefix(line, storageMarkerGen); ok {
				if n, err := strconv.ParseUint(g, 10, 64); err == nil {
					gens[p] = n
					maxGen = max(maxGen, n)
				}
				continue
			}
			if id, ok := strings.CutPrefix(line, storageMarkerNode); ok {
				ids[p] = id
				continue
			}
			if k, ok := strings.CutPrefix(line, storageMarkerKept); ok {
				g, kp, _ := strings.Cut(k, " ")
				if n, err := strconv.ParseUint(g, 10, 64); err == nil && kp != "" {
					kept[kp] = max(kept[kp], n)
				}
				continue
			}
			if line != "" {
				lines = append(lines, line)
			}
		}
		if len(ownIDs) > 0 && ids[p] != "" && !ownIDs[ids[p]] {
			continue
		}
		if g, ok := gens[p]; ok {
			ownMax = max(ownMax, g)
		}
		for kp, n := range kept {
			lastKept[kp] = max(lastKept[kp], n)
		}
		for _, l := range lines {
			known[l] = true
			listed[p] = true
		}
	}
	// A marker without a number is taken as current only while no marker
	// carries one, or when it is empty (the override the warning in dropUnmountedPaths tells
	// an operator to make). One listing paths beside numbered ones is the
	// copy release 2026.11.17.2 wrote onto a bare mountpoint (Y11).
	for p, g := range gens {
		if k, ok := lastKept[p]; g < ownMax && !(ok && g == k) {
			marked[p] = false
		}
	}
	if len(gens) > 0 {
		for p := range listed {
			if _, ok := gens[p]; !ok {
				marked[p] = false
			}
		}
	}
	// Only a current marker's id counts: a stale one may be the copy a boot on
	// the bare mountpoint wrote, naming the identity it enrolled there.
	nodeIDs = map[string]bool{}
	for p, id := range ids {
		if marked[p] && id != "" && (len(ownIDs) == 0 || ownIDs[id]) {
			nodeIDs[id] = true
		}
	}
	return known, marked, lastKept, maxGen, nodeIDs
}

// identityDirUnmounted reports whether the first storage path, which holds the
// node's identity (.node_id, .node_secret, .node_key), reads as a disk that did
// not mount, by dropUnmountedPaths' rule. It runs before anything reads or
// writes the identity: a node booting on the bare mountpoint finds no identity
// and enrols as a new node.
func identityDirUnmounted(pathsCSV string) bool {
	var paths []string
	for _, p := range strings.Split(pathsCSV, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return false
	}
	p := paths[0]
	// No own-id filter: this runs before enrolment, and the id on a bare first
	// mountpoint may be the phantom one this check is there to catch.
	known, marked, _, _, nodeIDs := readStorageMarkers(paths, nil)
	if marked[p] || !known[p] || !dirHoldsNothing(p) {
		return false
	}
	// The loud warning is for a fresh enrolment. Markers written by
	// 2026.11.17.2 and 2026.11.18 (gen-less, or gens without #kept lines) leave
	// a healthy first path reading as stale, and its identity tells it apart.
	if holdsNodeIdentity(p, nodeIDs) {
		log.Printf("storage: [warn] the first storage path %s reads as a disk that is not mounted (empty, without a current %s marker), but it holds this node's identity, so the node boots from it. Check that the disk is mounted", p, storageMarkerName)
		return false
	}
	return true
}

// identityDirWarning is the boot warning for a first storage path that reads
// as a disk that did not mount, "" when it does not. It never refuses the
// boot: every refusal shipped so far stopped a healthy node (R26-08 was a
// foreign disk's marker), and a node that is down takes every server with it.
func identityDirWarning(pathsCSV string) string {
	if !identityDirUnmounted(pathsCSV) {
		return ""
	}
	p := firstPersistedStoragePath(pathsCSV)
	return fmt.Sprintf("storage: [WARNING] the first storage path %s holds the node identity and reads as a disk that is not mounted (it holds no server and lacks a current %s marker another storage path expects). The node boots from it anyway. "+
		"If a disk belongs there, stop the node, mount the disk and restart: a node running on the bare mountpoint may enrol as a new node. If it is a new empty disk, replace %s with an empty file to clear this warning.",
		p, storageMarkerName, filepath.Join(p, storageMarkerName))
}

// holdsNodeIdentity reports whether dir holds an identity that proves it is the
// disk this node runs from. .node_key is written before the node enrols and
// .redis_addr is a cache, so neither stops a fresh enrolment and neither counts
// (R24-10). Once a current marker records the node id, only that id in .node_id
// counts: a boot that ran on the bare mountpoint left its own identity there
// (R24-11). Until then (markers from before 2026.11.19) either file counts.
//
// ponytail: on the first boot of this release a phantom .node_id/.node_secret
// on a bare first mountpoint still passes; every boot after it records the id.
func holdsNodeIdentity(dir string, nodeIDs map[string]bool) bool {
	if len(nodeIDs) > 0 {
		id, err := cachedNodeID(dir)
		return err == nil && nodeIDs[id]
	}
	for _, f := range []string{".node_id", ".node_secret"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	return false
}

// dropUnmountedPaths leaves out a path another marker lists, that has no
// marker of its own and holds nothing: a disk that did not mount, whose empty
// mountpoint passed every check above. Servers keyed there ran on an empty
// tree on the root disk, and new servers filled the OS disk. Every path kept is
// marked, and given the full list.
//
// ponytail: a single path, or a boot where every path is unmounted at once,
// has no other marker to vouch for it and is taken as before.
func (sm *StorageManager) dropUnmountedPaths(paths []string) []string {
	known, marked, lastKept, maxGen, nodeIDs := readStorageMarkers(paths, ownNodeIDs(nodeSecretDir))
	var kept []string
	for _, p := range paths {
		// The identity exception matches identityDirUnmounted: a path the node
		// booted from is kept, and keeping it rewrites its stale marker. The
		// identity dir itself is never re-judged: identityDirUnmounted already
		// let this boot run from it, and the enrolment since may have replaced
		// .node_id, which no recorded id matches yet (R25-09).
		if p != nodeSecretDir && !marked[p] && known[p] && dirHoldsNothing(p) && !holdsNodeIdentity(p, nodeIDs) {
			log.Printf("storage: [warn] %s was a storage path of this node and is now empty without a current %s marker, so its disk is most likely not mounted; skipping. If it is a new empty disk, replace %s with an empty file to use it", p, storageMarkerName, filepath.Join(p, storageMarkerName))
			sm.skipPath(p)
			continue
		}
		kept = append(kept, p)
		known[p] = true
		lastKept[p] = maxGen + 1
	}
	list := make([]string, 0, len(known))
	for p := range known {
		list = append(list, p)
	}
	sort.Strings(list)
	for _, p := range list {
		if g, ok := lastKept[p]; ok {
			list = append(list, storageMarkerKept+strconv.FormatUint(g, 10)+" "+p)
		}
	}
	// The id of the boot writing this, read from the kept path that holds it
	// (this runs after enrolment). Without one, the ids on record are kept.
	ids := nodeIDs
	for _, p := range kept {
		if id, err := cachedNodeID(p); err == nil && id != "" {
			ids = map[string]bool{id: true}
			break
		}
	}
	idLines := make([]string, 0, len(ids))
	for id := range ids {
		idLines = append(idLines, storageMarkerNode+id)
	}
	sort.Strings(idLines)
	list = append(list, idLines...)
	list = append(list, storageMarkerGen+strconv.FormatUint(maxGen+1, 10))
	body := []byte(strings.Join(list, "\n") + "\n")
	for _, p := range kept {
		if err := os.WriteFile(filepath.Join(p, storageMarkerName), body, 0o644); err != nil {
			log.Printf("storage: [warn] cannot write the storage marker on %s: %v", p, err)
		}
	}
	return kept
}

// ownNodeIDs are the ids this node's own markers may name: the .node_id in dir
// and the ids dir's marker records. The enrolment before dropUnmountedPaths
// may have replaced .node_id, and the markers still name the id this node ran
// as until now; judged by the new id alone, every own marker read as foreign
// and a bare mountpoint was taken and marked (R28-07). dir is the identity
// dir, which identityDirUnmounted already let this boot run from.
func ownNodeIDs(dir string) map[string]bool {
	ids := map[string]bool{}
	if dir == "" {
		return ids
	}
	if id, _ := cachedNodeID(dir); id != "" {
		ids[id] = true
	}
	if b, err := os.ReadFile(filepath.Join(dir, storageMarkerName)); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if id, ok := strings.CutPrefix(strings.TrimSpace(line), storageMarkerNode); ok && id != "" {
				ids[id] = true
			}
		}
	}
	return ids
}

// dirHoldsNothing reports whether dir holds no server: no directory but the
// node's own dot ones. Plain files do not count: an empty mountpoint carries
// the ones a boot that ran on it wrote (beacons, write test, and on the first
// path sftp_host_key, <uuid>.counted and <uuid>.deleted), and a server is a
// directory.
func dirHoldsNothing(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			return false
		}
	}
	return true
}

// SkippedPaths are the configured paths left out at boot, sorted. Servers
// on them exist but cannot be seen, so the heartbeat reports them: Core must
// not take a server missing from the listed paths for a dead row (Y10).
func (sm *StorageManager) SkippedPaths() []string {
	out := make([]string, 0, len(sm.skippedPaths))
	for p := range sm.skippedPaths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// UnavailablePaths are the paths whose servers infos cannot list: the ones
// skipped at boot and the ones whose directory could not be read now (a stale
// NFS handle, an I/O error). Core lists no server as missing from disk while
// any path is unavailable (R26-09). Sorted.
func (sm *StorageManager) UnavailablePaths(infos []StorageInfo) []string {
	out := sm.SkippedPaths()
	for _, info := range infos {
		if info.unreadable {
			out = append(out, info.Path)
		}
	}
	sort.Strings(out)
	return out
}

// readStorageKey returns the path the server's storage key names, "" for no
// key. An error is a Redis that could not answer, which is not "no key": the
// lookup then scanned and wrote its guess back as the key for good (Y09).
func (sm *StorageManager) readStorageKey(serverUUID string) (string, error) {
	// Bounded: every server path lookup runs this, several of them on the
	// mesh read loop, and a hung Redis held each for go-redis's ~12s of
	// retries. The deadline stops the retries (Y01).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	val, err := sm.rdb.Get(ctx, fmt.Sprintf("node:%s:server:%s:storage", nodeID, serverUUID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return val, err
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

	// No sm.mu: sm.paths is fixed after NewStorageManager, and a Redis GET or
	// os.Stat on a hung NFS path under the read lock, with a SetPlacement
	// queued behind it, blocked every server's path lookup on the node.

	// 1. Redis lookup
	keyed := false
	if sm.rdb != nil {
		if val, err := sm.readStorageKey(serverUUID); err != nil {
			// Unanswered is not unkeyed: the scan below is never written back,
			// and with a path skipped any copy it finds may be the leftover.
			log.Printf("storage: cannot read the storage key of %s: %v", serverUUID, err)
			keyed = true
			if sm.skipped > 0 {
				return ""
			}
		} else if val != "" {
			// Verify the path is still in our list
			for _, p := range sm.paths {
				if p == val {
					return val
				}
			}
			keyed = true
			// A copy elsewhere is then a leftover (a move killed in its source
			// delete), not the live tree, which waits on the unmounted disk.
			if sm.skippedPaths[val] {
				log.Printf("storage: Redis path %q for %s is configured but not available, not resolving", val, serverUUID)
				return ""
			}
			log.Printf("storage: Redis path %q for %s not in active paths, scanning...", val, serverUUID)
		}
	}

	// 2. Scan all paths for existing UUID directory
	holding := sm.PathsHolding(serverUUID)
	found, copies := "", len(holding)
	if copies > 0 {
		found = holding[0]
	}
	if found != "" {
		// The first copy scanned was the leftover a move left whenever it sat
		// on an earlier path, and every lookup during a Redis blip used it
		// (A08), so an unreadable key resolves to neither. A key that is
		// simply gone, as every key is after a Valkey restart, resolved to
		// neither for good too (R29-07), and nothing wrote it again (R30-09):
		// the copy the move markers name live is settled as the live one
		// instead. Not the newest mtime: a move's debris is written last
		// (R31-08, R31-09).
		if copies > 1 {
			if !keyed {
				if live := markedLiveCopy(serverUUID, holding); live != "" {
					log.Printf("storage: %s has %d copies and no storage key; the move markers name %s live, recording it (the other copies are left alone)", serverUUID, copies, live)
					sm.setRedisPath(serverUUID, live)
					return live
				}
				sm.noteAmbiguity(serverUUID, holding)
			}
			log.Printf("storage: %s has %d copies and no readable storage key, not resolving", serverUUID, copies)
			return ""
		}
		// Written back only when it cannot be a guess: a key naming a path
		// that is merely not mounted yet, or a second copy a move left, made
		// the first match the server's storage for good - and the next move
		// to the live path cleared the live tree as the "leftover" (K35).
		if !keyed && copies == 1 {
			sm.setRedisPath(serverUUID, found)
		}
		return found
	}

	// 3. Not found: the default is for a NEW server. One whose key names a
	// path that is not mounted lives there, and paths[0] had delete and
	// migrate_cleanup clear an empty directory, report success and drop the
	// key and limit while the real tree stayed behind.
	if keyed {
		return ""
	}
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

// PathsHolding are the storage paths holding a directory of the server, in
// configured order. Unlike GetServerPath it does not decide which is live.
func (sm *StorageManager) PathsHolding(serverUUID string) []string {
	if !validate.IsServerUUID(serverUUID) {
		return nil
	}
	var out []string
	for _, p := range sm.paths {
		if stat, err := os.Stat(filepath.Join(p, serverUUID)); err == nil && stat.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// storageLiveMarker and storageMovedAwayMarker are the node's proof, on disk,
// of which copy a storage move left live: the storage key is gone after a
// Valkey restart, and mtimes are no proof, because a move's own debris is
// written last (the source delete bumps its directories, a copy killed
// halfway is newer than its source). The target is marked live, with a
// generation above the source's, before the key flips; a copy is marked moved
// away before any of it is deleted. Both names are isProtectedFile's.
const (
	storageLiveMarker      = ".dylaris_live"
	storageMovedAwayMarker = ".dylaris_moved_away"
)

// parkedCopyInfix names a copy removeCopy parked: .<uuid>.moved-away-<ns>.
const parkedCopyInfix = ".moved-away-"

// removeTree is a var so a test can fail a delete partway.
var removeTree = os.RemoveAll

// removeCopy deletes a copy of a server that is not its live one, marking it
// moved away first so that a delete stopped partway leaves no candidate.
// Unmarked, it is not touched: a half-deleted copy without the mark is the
// debris a lookup could settle on. A missing dir is nothing to remove.
//
// A mark that cannot be written (ENOSPC on a full XFS disk, EDQUOT at the
// server's project limit: the usual reasons to move a server off) parks the
// copy under a hidden name instead. A rename needs no free blocks, and outside
// its <uuid> name the copy is no candidate either (R32-10). What a failed
// delete leaves of it goes at the next node start (sweepInterruptedWork).
func removeCopy(dir string) error {
	if err := writeServerRootFile(dir, storageMovedAwayMarker, []byte(strconv.FormatInt(time.Now().UnixNano(), 10))); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		parked := filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+parkedCopyInfix+strconv.FormatInt(time.Now().UnixNano(), 10))
		if rerr := os.Rename(dir, parked); rerr != nil {
			return fmt.Errorf("mark %s as moved away: %w (and park it: %v)", dir, err, rerr)
		}
		dir = parked
	}
	return removeTree(dir)
}

// clearMovedAway drops dir's moved-away mark, for a copy the key names live.
func clearMovedAway(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove(storageMovedAwayMarker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// liveGeneration is the generation in dir's live mark; ok=false with no
// error when there is none.
func liveGeneration(dir string) (gen int64, ok bool, err error) {
	b, err := readServerRootFile(dir, storageLiveMarker)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	gen, err = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return gen, err == nil, err
}

// markedLiveCopy is the path whose copy of the server the move markers name
// live, or "" when they do not settle it: a copy marked moved away never wins;
// of the rest, the only one, or the only one with the newest live mark. Copies
// an older node left carry no marks and stay unsettled. Any mark that cannot
// be read leaves it unsettled too.
func markedLiveCopy(serverUUID string, paths []string) string {
	var standing []string
	gens := map[string]int64{}
	for _, p := range paths {
		dir := filepath.Join(p, serverUUID)
		if _, err := readServerRootFile(dir, storageMovedAwayMarker); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return ""
		}
		standing = append(standing, p)
		gen, ok, err := liveGeneration(dir)
		if err != nil {
			return ""
		}
		if ok {
			gens[p] = gen
		}
	}
	if len(standing) == 1 {
		return standing[0]
	}
	best, tie := "", false
	for _, p := range standing {
		gen, ok := gens[p]
		switch {
		case !ok:
		case best == "" || gen > gens[best]:
			best, tie = p, false
		case gen == gens[best]:
			tie = true
		}
	}
	if tie {
		return ""
	}
	return best
}

// StorageAmbiguity is a server with copies on several storage paths that no
// storage key and no move marker tell apart. The heartbeat reports it so an
// operator can remove the stale tree; until then the server resolves nowhere.
type StorageAmbiguity struct {
	Server string   `json:"server"`
	Paths  []string `json:"paths"`
}

func (sm *StorageManager) noteAmbiguity(serverUUID string, paths []string) {
	sm.ambMu.Lock()
	defer sm.ambMu.Unlock()
	if sm.ambiguous == nil {
		sm.ambiguous = map[string][]string{}
	}
	sm.ambiguous[serverUUID] = append([]string(nil), paths...)
}

// forgetAmbiguity drops the report once the server has a key or is gone.
func (sm *StorageManager) forgetAmbiguity(serverUUID string) {
	sm.ambMu.Lock()
	delete(sm.ambiguous, serverUUID)
	sm.ambMu.Unlock()
}

// StorageAmbiguities are the servers GetServerPath could not resolve between
// copies, sorted. Not re-checked on disk: the heartbeat reads this, and a
// stat on a hung path stopped every beat (R28-08). The next lookup of the
// server, once a copy is gone, writes its key and clears the report.
func (sm *StorageManager) StorageAmbiguities() []StorageAmbiguity {
	sm.ambMu.Lock()
	out := make([]StorageAmbiguity, 0, len(sm.ambiguous))
	for u, paths := range sm.ambiguous {
		out = append(out, StorageAmbiguity{Server: u, Paths: paths})
	}
	sm.ambMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Server < out[j].Server })
	return out
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
func pickByPriority(ordered []string) string {
	for _, p := range ordered {
		if getFreeSpace(p) > 0 {
			return p
		}
	}
	return ""
}

// SelectStoragePath picks the path for a NEW server: an explicit request wins,
// then the admin's manual order if configured, then most free space.
// After selection, it creates the server directory and stores the assignment in Redis.
//
// sm.mu covers only the placement policy: statfs and MkdirAll on a hung NFS
// path never return, and under the lock they stalled every GetServerPath on
// the node. sm.paths is fixed after NewStorageManager.
func (sm *StorageManager) SelectStoragePath(serverUUID string, preferredPath string) (string, error) {
	// If admin explicitly chose a path, use it (if valid)
	if preferredPath != "" {
		for _, p := range sm.paths {
			if p == preferredPath {
				return sm.assignPath(serverUUID, p)
			}
		}
		return "", fmt.Errorf("preferred path %q not available", preferredPath)
	}

	sm.mu.RLock()
	manual := sm.placementMode == placementManual
	ordered := sm.orderedPaths()
	sm.mu.RUnlock()

	if manual {
		if p := pickByPriority(ordered); p != "" {
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
	sm.forgetAmbiguity(serverUUID)
	if sm.rdb != nil {
		key := fmt.Sprintf("node:%s:server:%s:storage", nodeID, serverUUID)
		sm.rdb.Del(context.Background(), key)
	}
}

// GetStorageInfo returns disk usage info for all storage paths.
//
// No sm.mu: sm.paths is fixed after NewStorageManager, and statfs or ReadDir
// on a hung NFS path held the read lock forever, which with a SetPlacement
// queued behind it blocked every server's path lookup on the node.
//
// Each path is probed in its own goroutine and waited on for at most
// storageProbeTimeout: the heartbeat calls this, and a hard NFS mount that
// stops answering blocks statfs for good, which stopped every beat and read the
// node offline (R28-08). A path not back in time reads as unreadable, so Core
// lists it under storageUnavailable, and a probe still stuck is waited on again
// rather than joined by another.
func (sm *StorageManager) GetStorageInfo() []StorageInfo {
	probes := make([]*storageProbe, len(sm.paths))
	for i, p := range sm.paths {
		probes[i] = sm.probePath(p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), storageProbeTimeout)
	defer cancel()
	infos := make([]StorageInfo, 0, len(sm.paths))
	for i, pr := range probes {
		select {
		case <-pr.done:
			sm.probeMu.Lock()
			pr.served = true
			sm.probeMu.Unlock()
			infos = append(infos, pr.info)
		case <-ctx.Done():
			// A probe slower than the window but done within a beat or two
			// is a slow path, not a dead one: zeros on every second beat had
			// Core skip it for placement and disk checks (R30-07).
			if pr.prev != nil && time.Since(pr.started) < storageProbeStale {
				infos = append(infos, *pr.prev)
				continue
			}
			log.Printf("storage: [warn] %s did not answer statfs/readdir within %s; reporting it unavailable", sm.paths[i], storageProbeTimeout)
			infos = append(infos, StorageInfo{Path: sm.paths[i], ServerUUIDs: []string{}, unreadable: true})
		}
	}
	return infos
}

// storageProbeTimeout bounds GetStorageInfo; well under the heartbeat's 5 s
// tick, so a hung path still leaves the discovery key its 15 s TTL.
var storageProbeTimeout = 2 * time.Second

// storageProbeStale is how long a probe may run while the previous one's
// result stands in for it; past that the path reads unavailable.
var storageProbeStale = 10 * time.Second

// storageProbe is one statStoragePath run; info is set before done is closed.
// served (under probeMu) says a GetStorageInfo has reported info. prev is the
// result of the probe this one replaced and started when it began, both set
// before the probe is shared and never changed.
type storageProbe struct {
	done    chan struct{}
	info    StorageInfo
	served  bool
	prev    *StorageInfo
	started time.Time
}

// probePath starts a probe of p, or returns the one still running on it or
// finished but not yet reported.
func (sm *StorageManager) probePath(p string) *storageProbe {
	sm.probeMu.Lock()
	defer sm.probeMu.Unlock()
	if pr := sm.probes[p]; pr != nil {
		select {
		case <-pr.done:
			// Finished after its beat gave up on it: report it now. Restarted
			// instead, a path that needs longer than storageProbeTimeout read
			// unavailable with zero capacity on every beat (R29-08).
			if !pr.served {
				return pr
			}
		default:
			return pr
		}
	}
	if sm.probes == nil {
		sm.probes = map[string]*storageProbe{}
	}
	pr := &storageProbe{done: make(chan struct{}), started: time.Now()}
	if old := sm.probes[p]; old != nil {
		info := old.info
		pr.prev = &info
	}
	sm.probes[p] = pr
	go func() {
		pr.info = statStoragePath(p)
		close(pr.done)
	}()
	return pr
}

// statStoragePath reads p's disk usage and the server directories on it.
func statStoragePath(p string) StorageInfo {
	info := StorageInfo{Path: p}

	total, free := diskSpaceOf(p)
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
	} else {
		info.unreadable = true
	}
	return info
}

// setRedisPath stores the storage path for a server in Redis (no expiry).
func (sm *StorageManager) setRedisPath(serverUUID, storagePath string) error {
	// Whatever path is recorded, the server is settled and no longer reported.
	sm.forgetAmbiguity(serverUUID)
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

// diskSpaceOf is getDiskSpace; a test swaps it for a statfs that hangs.
var diskSpaceOf = getDiskSpace

// getFreeSpace returns the available bytes on the filesystem containing path.
func getFreeSpace(path string) uint64 {
	_, free := diskSpaceOf(path)
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
// path. No sm.mu: sm.paths is fixed after NewStorageManager, and a stat on a
// hung NFS path under the lock stalled every other server's path lookup on the
// node. migrate_storage of one server is already serialised by the command
// in-flight guard.
func (sm *StorageManager) resolveMigration(serverUUID, targetPath string) (string, error) {
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
		val, err := sm.readStorageKey(serverUUID)
		if err != nil {
			return "", fmt.Errorf("read the storage key of %s: %w", serverUUID, err)
		}
		// As in GetServerPath: the live tree waits on the unmounted disk, and
		// the scan below took the leftover a killed move left elsewhere.
		if sm.skippedPaths[val] {
			return "", fmt.Errorf("server %s lives on %q, which is configured but not mounted; refusing the move", serverUUID, val)
		}
		for _, p := range sm.paths {
			if p == val {
				currentPath = val
				break
			}
		}
	}
	scanned := currentPath == ""
	if scanned {
		var copies []string
		for _, p := range sm.paths {
			if stat, err := os.Stat(filepath.Join(p, serverUUID)); err == nil && stat.IsDir() {
				copies = append(copies, p)
			}
		}
		// Only the storage key can vouch for which copy is live. Refused
		// before the already-on-path answer too: with the first copy on the
		// target, that answer finished a move that never ran, and the finish
		// force-removed the container running on the other copy.
		if len(copies) > 1 {
			return "", fmt.Errorf("server %s has a directory on %s and no storage key says which one is live; refusing the move", serverUUID, strings.Join(copies, ", "))
		}
		if len(copies) == 1 {
			currentPath = copies[0]
		}
	}
	if currentPath == "" {
		return "", fmt.Errorf("server %s not found on any storage path", serverUUID)
	}
	if currentPath == targetPath {
		return "", fmt.Errorf("server %s %w %q", serverUUID, errAlreadyOnPath, targetPath)
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

// errAlreadyOnPath is a move whose target is where the server already lives.
// It is also what a move killed after its key flip reads like on redelivery.
var errAlreadyOnPath = errors.New("is already on path")

// errStorageKeyUnsure is a move whose key write failed after the copy and
// could not be read back: the key may name either copy. Both are whole, and a
// start goes to the one the key names (see boundToMovedCopy).
var errStorageKeyUnsure = errors.New("record the new storage path")

// keyedPath is the storage path the server's storage key names, when that is
// one of this node's paths, else "". Unlike GetServerPath it never guesses by
// scanning: only the key can vouch for which of two copies is live.
func (sm *StorageManager) keyedPath(serverUUID string) string {
	if sm == nil || sm.rdb == nil || !validate.IsServerUUID(serverUUID) {
		return ""
	}
	if ch := sm.moveInProgress(serverUUID); ch != nil {
		<-ch
	}
	live, err := sm.rdb.Get(context.Background(), fmt.Sprintf("node:%s:server:%s:storage", nodeID, serverUUID)).Result()
	if err != nil || live == "" {
		return ""
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for _, p := range sm.paths {
		if p == live {
			return live
		}
	}
	return ""
}

// dropStaleCopies removes the server's directories on every storage path but
// the one its storage key names: a move killed during the delete of its
// source left that copy behind. Only the key can vouch for which copy is live,
// so without one nothing is removed.
func (sm *StorageManager) dropStaleCopies(serverUUID string) {
	live := sm.keyedPath(serverUUID)
	if live == "" {
		return
	}
	sm.mu.RLock()
	paths := append([]string(nil), sm.paths...)
	sm.mu.RUnlock()
	for _, p := range paths {
		dir := filepath.Join(p, serverUUID)
		if p == live {
			// A move killed between its key flip and clearing the target's
			// mark left the live copy reading as moved away.
			if err := clearMovedAway(dir); err != nil {
				log.Printf("storage: [warn] could not clear the moved-away mark of the live copy %s: %v", dir, err)
			}
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		log.Printf("storage: removing the leftover copy of %s at %s (it lives on %s)", serverUUID, dir, live)
		if err := removeCopy(dir); err != nil {
			log.Printf("storage: [warn] could not remove leftover copy %s: %v", dir, err)
		}
	}
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
	if err := sm.holdsLocked(serverUUID); err != nil {
		sm.movingMu.Unlock()
		return nil, err
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

// holdsLocked is the refusal a local move makes for writers and folder
// downloads. Under movingMu.
func (sm *StorageManager) holdsLocked(serverUUID string) error {
	if n := sm.writers[serverUUID]; n > 0 {
		return writersInProgress(serverUUID, n)
	}
	if n := sm.readers[serverUUID]; n > 0 {
		return fmt.Errorf("server %s has %d folder download(s) in progress; retry the move once they finish", serverUUID, n)
	}
	return nil
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
	err = sm.holdsLocked(serverUUID)
	sm.movingMu.Unlock()
	if err != nil {
		return err
	}
	// The move clears a leftover copy at the target before its own check.
	dstDir := filepath.Join(targetPath, serverUUID)
	_, err = checkMigrationSpace(filepath.Join(currentPath, serverUUID), targetPath, uint64(max(dirSize(dstDir), 0)))
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

// holdReader registers a folder zip download until the returned func runs.
// Its walk reads through a Root on the source tree, and a move deleted that
// tree under it: every vanished file was skipped and the truncated zip was
// reported complete. A single file needs none, its open fd survives the
// unlink. Taken before the path resolves, as holdWriter is.
func (sm *StorageManager) holdReader(serverUUID string) func() {
	if sm == nil {
		return func() {}
	}
	sm.movingMu.Lock()
	if sm.readers == nil {
		sm.readers = map[string]int{}
	}
	sm.readers[serverUUID]++
	sm.movingMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			sm.movingMu.Lock()
			if sm.readers[serverUUID]--; sm.readers[serverUUID] <= 0 {
				delete(sm.readers, serverUUID)
			}
			sm.movingMu.Unlock()
		})
	}
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

// isMoving reports whether a lookup of this server would wait for a move. A
// fence set right after the check still makes the caller wait; it only keeps
// the requests that arrive during a move from each waiting out all of it.
func (sm *StorageManager) isMoving(serverUUID string) bool {
	return sm != nil && sm.moveInProgress(serverUUID) != nil
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
	if err := removeCopy(dstDir); err != nil {
		return fmt.Errorf("clear the leftover copy at %s: %w", dstDir, err)
	}

	srcSize, err := checkMigrationSpace(srcDir, targetPath, 0)
	if err != nil {
		return err
	}

	log.Printf("storage: migrating %s: %s → %s (%s)", serverUUID, currentPath, targetPath, formatBytes(uint64(srcSize)))

	// The target reads as moved away until its key is written: a move killed
	// during the copy, or one whose key write is unconfirmed, then never
	// outranks the source after a key loss. The copy brings the source's live
	// mark along, which tied with it or won (R32-11, R32-09).
	// With the source's mode, as the copy would have made it.
	srcInfo, err := os.Stat(srcDir)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dstDir, srcInfo.Mode().Perm()); err != nil {
		return fmt.Errorf("create %s: %w", dstDir, err)
	}
	if err := writeServerRootFile(dstDir, storageMovedAwayMarker, []byte(strconv.FormatInt(time.Now().UnixNano(), 10))); err != nil {
		removeCopy(dstDir)
		return fmt.Errorf("mark %s as not yet live: %w", dstDir, err)
	}

	// copyTree, not copyDir: this is a move, so the destination has to be the
	// whole server including the entries copyDir withholds from a user-facing
	// duplicate. With backups present copyDir loses enough bytes to fail the
	// 90% check below; without them it passes and the source is deleted, taking
	// .active_server and .node_config.json with it.
	if err := migrateCopyTree(srcDir, dstDir); err != nil {
		removeCopy(dstDir)
		return fmt.Errorf("copy failed: %v", err)
	}

	// Verify: dest must be at least 90% of source size
	if dstSize := dirSize(dstDir); srcSize > 0 && dstSize < srcSize*9/10 {
		removeCopy(dstDir)
		return fmt.Errorf("copy verification failed: source=%s, dest=%s", formatBytes(uint64(srcSize)), formatBytes(uint64(dstSize)))
	}

	// Marked before the key flips, above the source's own mark, which the
	// copy brought along: once the moved-away mark goes, after a key loss this
	// is what tells the whole target from the source (R31-08, R31-09).
	gen := time.Now().UnixNano()
	if srcGen, ok, _ := liveGeneration(srcDir); ok && srcGen >= gen {
		gen = srcGen + 1
	}
	if err := writeServerRootFile(dstDir, storageLiveMarker, []byte(strconv.FormatInt(gen, 10))); err != nil {
		removeCopy(dstDir)
		return fmt.Errorf("mark %s live: %w", dstDir, err)
	}

	// The key moves before the source goes: a node killed during the delete,
	// or a key that could not be written, left the server resolving to a
	// half-deleted tree. Both copies are whole here, so on a failed write
	// neither is touched, whichever one the key ended up naming.
	if err := sm.setRedisPath(serverUUID, targetPath); err != nil {
		// A reply that timed out after Redis applied the write. Only a read
		// naming the target settles it: one naming the source may still be
		// overtaken by the write in flight.
		if v, gerr := sm.rdb.Get(context.Background(), fmt.Sprintf("node:%s:server:%s:storage", nodeID, serverUUID)).Result(); gerr != nil || v != targetPath {
			return fmt.Errorf("%w: %w", errStorageKeyUnsure, err)
		}
	}
	if err := clearMovedAway(dstDir); err != nil {
		// The key names the target, so it is live; only a later key loss
		// reads it wrong, and finishStorageMove clears it again.
		log.Printf("storage: [warn] could not clear the moved-away mark of %s: %v", dstDir, err)
	}

	// Remove old directory (data is safe at target)
	if err := removeCopy(srcDir); err != nil {
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
