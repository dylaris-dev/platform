package main

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// deletedServers remembers which servers were deleted on this node, so work
// that was already running for one cannot bring it back.
//
// The node runs commands in parallel, eight at a time, and nothing orders a
// delete against a setup, reinstall or move of the same server. A delete that
// arrived during a minutes-long Forge install removed the container and the
// directory; the install then recreated the directory and ended by creating and
// STARTING a container for a server Core no longer had. Nothing removes a
// container without a database row, so it kept its memory and its port.
//
// A UUID is never reused, so a tombstone is final; it is only aged out so the
// map and the directory stay small. The lifetime is not "how long a command
// runs": the queue redelivers a pending setup however old it is, so a node that
// was off for days still sees it. A day was too short for a weekend outage.
//
// The map alone forgot every delete on a restart, while the queue redelivers
// the setup that was still running (it was never ACKed). So each tombstone is
// also a <uuid>.deleted file beside the node secret, which survives it.
var deletedServers = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

const deletedServerMemory = 90 * 24 * time.Hour

func markServerDeleted(uuid string) {
	deletedServers.Lock()
	defer deletedServers.Unlock()
	now := time.Now()
	for u, t := range deletedServers.at {
		if now.Sub(t) > deletedServerMemory {
			delete(deletedServers.at, u)
		}
	}
	deletedServers.at[uuid] = now
	if nodeSecretDir == "" {
		return
	}
	if old, _ := filepath.Glob(filepath.Join(nodeSecretDir, "*.deleted")); len(old) > 0 {
		for _, p := range old {
			if fi, err := os.Stat(p); err == nil && now.Sub(fi.ModTime()) > deletedServerMemory {
				os.Remove(p)
			}
		}
	}
	if err := os.WriteFile(tombstonePath(uuid), nil, 0o600); err != nil {
		log.Printf("delete %s: tombstone not persisted, a restart forgets it: %v", uuid, err)
	}
}

func serverDeleted(uuid string) bool {
	deletedServers.Lock()
	t, ok := deletedServers.at[uuid]
	deletedServers.Unlock()
	if ok {
		return time.Since(t) <= deletedServerMemory
	}
	if nodeSecretDir == "" {
		return false
	}
	fi, err := os.Stat(tombstonePath(uuid))
	return err == nil && time.Since(fi.ModTime()) <= deletedServerMemory
}

// Beside the server directories rather than in a subdirectory: every sweep of
// a storage root skips plain files, and a ".deleted" directory would be walked
// as a server by the sweeps that do not skip dot names.
func tombstonePath(uuid string) string { return filepath.Join(nodeSecretDir, uuid+".deleted") }

// removeDeletedLeftovers removes this node's containers of servers deleted
// here and returns the others. A delete whose removal failed (a busy overlay
// unmount, a daemon timeout) still dropped the data and was acked, and Core
// had dropped the row, so nothing would name that container again; a build
// that slipped past the tombstone left one too. The reconciler retries the
// removal on every pass for as long as the tombstone lasts.
func (dm *DockerManager) removeDeletedLeftovers(existing []MCContainerInfo) []MCContainerInfo {
	var kept []MCContainerInfo
	for _, c := range existing {
		if !serverDeleted(c.UUID) {
			kept = append(kept, c)
			continue
		}
		if err := dm.PowerAction(c.UUID, "delete"); err != nil {
			log.Printf("reconciler(deleted): container %s of a deleted server not removed, retrying next pass: %v", c.ContainerName, err)
		} else {
			log.Printf("reconciler(deleted): removed container %s of a deleted server", c.ContainerName)
		}
	}
	return kept
}
