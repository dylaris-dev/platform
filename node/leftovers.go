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
				// A migration download lands next to the server dirs.
				if strings.HasSuffix(e.Name(), ".migration-in.zip") && olderThan(e, cutoff) {
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

// sweepServerDir sweeps one server directory to the depth these jobs write at:
// the server itself, its sub-servers, and one level below for uploads.
func sweepServerDir(dir string, cutoff time.Time) int {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0
	}
	defer root.Close()
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
			if isLeftover(e.Name(), e.IsDir(), depth, rel) {
				if olderThan(e, cutoff) && root.RemoveAll(name) == nil {
					removed++
				}
				continue
			}
			if e.IsDir() && e.Type()&os.ModeSymlink == 0 && depth < 3 {
				walk(name, depth+1)
			}
		}
	}
	walk(".", 1)
	return removed
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
