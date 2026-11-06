package main

import (
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// leftoverMinAge keeps the boot sweep away from anything still in use. A job
// of this process is seconds old when the sweep runs, but a directory's mtime
// only moves when an entry is added directly in it, so a long copy or download
// can look idle; six hours is past any of them.
const leftoverMinAge = 6 * time.Hour

// partialBackupSuffix marks a node-local backup archive still being written.
const partialBackupSuffix = ".partial"

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
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0
	}
	defer root.Close()
	uuid := filepath.Base(dir)
	removed := 0
	var walk func(rel string, depth int)
	walk = func(rel string, depth int) {
		f, err := root.Open(rel)
		if err != nil {
			return
		}
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return
		}
		for _, e := range entries {
			name := path.Join(rel, e.Name())
			if strings.HasPrefix(e.Name(), renameStagePrefix) && !strings.HasPrefix(e.Name(), renameStageOwn) {
				recoverParkedRename(root, rel, e)
				continue
			}
			if isLeftover(e.Name(), e.IsDir(), depth, rel) {
				if removeLeftover(root, name, e, cutoff, uuid) {
					removed++
				}
				continue
			}
			if e.IsDir() && e.Type()&os.ModeSymlink == 0 {
				walk(name, depth+1)
			}
		}
	}
	walk(".", 1)
	return removed
}

// recoverParkedRename gives back what a rename killed between its two renameat
// calls left under renameStagePrefix, which every tenant path hides and
// refuses. Same directory, visible name; a link is removed instead, since
// renameNoFollow refuses to move one and parks it only to judge it.
func recoverParkedRename(root *os.Root, rel string, e os.DirEntry) {
	name := path.Join(rel, e.Name())
	if e.Type()&os.ModeSymlink != 0 {
		_ = root.Remove(name)
		return
	}
	back := path.Join(rel, "recovered-"+strings.TrimPrefix(e.Name(), renameStagePrefix))
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
	case isDir && depth == 1 && strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".copying"):
		return true
	case !isDir && strings.HasPrefix(name, ".upload-") && strings.HasSuffix(name, ".tmp"):
		return true
	case !isDir && path.Base(parent) == backupDirName && strings.HasSuffix(name, partialBackupSuffix):
		return true
	}
	return false
}

// sharedWithAnotherNode reports whether another node has claimed this storage
// path within a day (see shared_storage_beacon.go).
func sharedWithAnotherNode(base string) bool {
	for _, b := range readBeacons(base) {
		if b.NodeID != nodeID && time.Since(b.UpdatedAt) < 24*time.Hour {
			return true
		}
	}
	return false
}

func olderThan(e os.DirEntry, cutoff time.Time) bool {
	info, err := e.Info()
	return err == nil && info.ModTime().Before(cutoff)
}
