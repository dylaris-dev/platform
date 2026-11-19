package main

import (
	"encoding/hex"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"dylaris-pkg/validate"
)

// leftoverMinAge keeps the boot sweep away from anything still in use. A job
// of this process is seconds old when the sweep runs, but a directory's mtime
// only moves when an entry is added directly in it, so a long copy or download
// can look idle; six hours is past any of them.
const leftoverMinAge = 6 * time.Hour

// partialBackupSuffix marks a node-local backup archive still being written.
const partialBackupSuffix = ".partial"

// maxTenantWalkDepth caps how deep a sweep descends into a tenant's tree; no
// job leaves anything that deep. Same cap as the quota walk.
var maxTenantWalkDepth = 4096

// sweepInterruptedWork removes what jobs killed by a node restart left behind.
// A node update stops the agent within seconds, whatever it was doing, and
// these were only removed by the job's own deferred cleanup, which never ran:
// a Technic install's download and unpack dirs, a sub-server copy, a migration
// download, a panel upload, a half-written node-local backup. All of them sit
// in the tenant's directory and count against its disk limit, and the tenant
// can neither see nor delete most of them.
//
// Only these exact name shapes, and only past leftoverMinAge. Removal goes
// through os.Root per server, so a tenant who swaps a directory for a link
// cannot point it outside their server.
func sweepInterruptedWork(roots []string) {
	removed := 0
	cutoff := time.Now().Add(-leftoverMinAge)
	for _, base := range roots {
		// Storage another node also uses: its jobs are not ours to judge.
		if sharedWithAnotherNode(base) {
			continue
		}
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				// A migration download lands next to the server dirs, under
				// pull's temp name until its last byte. No transfer survives a
				// restart, and the redelivered one budgets against the space
				// this one holds, so anything from before this process goes.
				if (strings.HasSuffix(e.Name(), ".migration-in.zip") || strings.HasPrefix(e.Name(), ".migration-pull-")) && olderThan(e, nodeStartedAt) {
					if os.Remove(filepath.Join(base, e.Name())) == nil {
						removed++
					}
				}
				continue
			}
			// A copy removeCopy parked is no server's: only its delete failed.
			if strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), parkedCopyInfix) {
				if removeTree(filepath.Join(base, e.Name())) == nil {
					removed++
				}
				continue
			}
			removed += sweepServerDir(filepath.Join(base, e.Name()), cutoff)
		}
	}
	if removed > 0 {
		log.Printf("cleanup: removed %d leftovers of jobs interrupted by a node restart", removed)
	}
}

// sweepServerDir sweeps one whole server directory: a panel upload stages next
// to its destination and a rename parks next to its target, at any depth.
func sweepServerDir(dir string, cutoff time.Time) int {
	uuid := filepath.Base(dir)
	removed := 0
	walkTenantTree(dir, func(e tenantEntry) bool {
		if strings.HasPrefix(e.Name(), renameStagePrefix) && !strings.HasPrefix(e.Name(), renameStageOwn) {
			recoverParkedRename(e.level, e)
			return false
		}
		if isLeftover(e.Name(), e.IsDir(), e.depth, e.parent()) {
			if removeLeftover(e.level, e.Name(), e, cutoff, uuid) {
				removed++
			}
			return false
		}
		return true
	})
	return removed
}

// tenantEntry is one entry walkTenantTree visits.
type tenantEntry struct {
	os.DirEntry
	level *os.Root // the directory holding it
	depth int      // 1 = directly in the walked directory
	above []string // own names of the directories above it, outermost first
}

// parent is the own name of the directory holding the entry, "." at the top.
func (e tenantEntry) parent() string {
	if len(e.above) == 0 {
		return "."
	}
	return e.above[len(e.above)-1]
}

// rel is the entry's slash path below the walked directory.
func (e tenantEntry) rel() string {
	return path.Join(path.Join(e.above...), e.Name())
}

// walkTenantTree visits every entry below dir, a tenant's tree, and descends
// into a directory (never a link) when visit returns true, down to
// maxTenantWalkDepth. Each level opens its children relative to its own
// handle and carries only its own name: re-resolving the full path from the
// top cost one openat per component per level, quadratic in a tenant's
// nesting (R37-09). Every sweep of a tenant's tree walks through here.
func walkTenantTree(dir string, visit func(tenantEntry) bool) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return
	}
	defer root.Close()
	var above []string
	var walk func(level *os.Root, depth int)
	walk = func(level *os.Root, depth int) {
		f, err := level.Open(".")
		if err != nil {
			return
		}
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return
		}
		for _, e := range entries {
			if !visit(tenantEntry{e, level, depth, above}) || !e.IsDir() || e.Type()&os.ModeSymlink != 0 || depth >= maxTenantWalkDepth {
				continue
			}
			child, closeChild, err := openTenantLevel(level, e.Name())
			if err != nil {
				continue
			}
			above = append(above, e.Name())
			walk(child, depth+1)
			above = above[:len(above)-1]
			closeChild()
		}
	}
	walk(root, 1)
}

// openTenantLevel opens directory name in level as a Root of its own. A Root
// opened from another is named by the two names joined, and every entry read
// from it keeps a copy of that name, so a chain N levels deep held O(N^2)
// bytes of names: gigabytes for a 4096-deep chain of long names (R38-06).
// Where it can, the child is reached through a name that stays short at any
// depth instead (openLevelByFd); close releases what it opened.
func openTenantLevel(level *os.Root, name string) (r *os.Root, close func(), err error) {
	if r, close, ok := openLevelByFd(level, name); ok {
		return r, close, nil
	}
	if r, err = level.OpenRoot(name); err != nil {
		return nil, nil, err
	}
	return r, func() { r.Close() }, nil
}

// recoverParkedRename gives back what a rename killed between its two renameat
// calls left under renameStagePrefix, which every tenant path hides and
// refuses. Same directory, visible name; a link is removed instead, since
// renameNoFollow refuses to move one and parks it only to judge it.
func recoverParkedRename(root *os.Root, e os.DirEntry) {
	name := e.Name()
	if e.Type()&os.ModeSymlink != 0 {
		_ = root.Remove(name)
		return
	}
	back := "recovered-" + strings.TrimPrefix(e.Name(), renameStagePrefix)
	if _, err := root.Lstat(back); err == nil {
		return
	}
	if err := root.Rename(name, back); err == nil {
		log.Printf("cleanup: an interrupted rename left %s parked, given back as %s", name, back)
	}
}

// removeLeftover removes one leftover past cutoff. A .partial archive goes
// whatever its age unless a backup of the server is running: no backup outlives
// a restart, and its mtime is the moment the node died, seconds before this
// sweep, so the age check kept it until a restart six hours later. Checked and
// removed under serverWorkMu, since a backup marks itself running before it
// creates its .partial.
func removeLeftover(root *os.Root, name string, e os.DirEntry, cutoff time.Time, uuid string) bool {
	if !e.IsDir() && strings.HasSuffix(e.Name(), partialBackupSuffix) {
		serverWorkMu.Lock()
		defer serverWorkMu.Unlock()
		if !olderThan(e, cutoff) && backupsInFlight.has("server:"+uuid) {
			return false
		}
		return root.Remove(name) == nil
	}
	return olderThan(e, cutoff) && root.RemoveAll(name) == nil
}

// isLeftover reports whether an entry at depth (1 = in the server directory)
// is one of the names an interrupted job leaves.
func isLeftover(name string, isDir bool, depth int, parent string) bool {
	switch {
	case isDir && depth <= 2 && (strings.HasPrefix(name, ".technic-stage-") || strings.HasPrefix(name, ".technic-dl-")):
		return true
	// Only the exact names the node writes (copy_sub_server's staging dir,
	// createTempIn's 16 hex digits): the tenant owns this tree and may hold a
	// .upload-cache.tmp or a .my.mods.copying of its own (R39-07).
	case isDir && depth == 1 && strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".copying") &&
		validate.IsSubServerName(strings.TrimSuffix(strings.TrimPrefix(name, "."), ".copying")):
		return true
	case !isDir && strings.HasPrefix(name, ".upload-") && strings.HasSuffix(name, ".tmp") &&
		isHex16(strings.TrimSuffix(strings.TrimPrefix(name, ".upload-"), ".tmp")):
		return true
	case !isDir && path.Base(parent) == backupDirName && strings.HasSuffix(name, partialBackupSuffix):
		return true
	}
	return false
}

func isHex16(s string) bool {
	if len(s) != 16 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// sharedWithAnotherNode reports whether another node has claimed this storage
// path within a day (see shared_storage_beacon.go).
func sharedWithAnotherNode(base string) bool {
	for _, b := range readBeacons(base) {
		if b.NodeID != nodeID && !formerOwnBeacon(b) && time.Since(b.UpdatedAt) < 24*time.Hour {
			return true
		}
	}
	return false
}

func olderThan(e os.DirEntry, cutoff time.Time) bool {
	info, err := e.Info()
	return err == nil && info.ModTime().Before(cutoff)
}
