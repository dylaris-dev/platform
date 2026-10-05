package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	reconcileInterval = 15 * time.Second

	// Crash policy, mirrored from log-shipper/main.go so both halves of the same
	// escalation read the same. Three restarts inside one window, then stop.
	//
	// The window replaces an aliveThreshold that reset the count after any run
	// longer than 60s. That looked equivalent and was not: a container surviving
	// 61 seconds each time reset forever and was restarted forever. Anchoring the
	// streak to wall clock instead means three crashes an hour apart are three
	// separate incidents, while three inside a quarter of an hour are a loop.
	maxCrashRetries = 3
	crashWindow     = 15 * time.Minute
)

// backoff durations indexed by crash count (0-based). One entry per allowed
// retry: with maxCrashRetries at 3 the whole escalation is over in about a
// minute and a half of backoff, and the last two entries of the old five-entry
// list were unreachable.
var backoffDurations = []time.Duration{
	0,
	30 * time.Second,
	60 * time.Second,
}

type reconcileInfo struct {
	crashCount    int
	lastRestart   time.Time
	lastSeenAlive time.Time
	streakStart   time.Time // when the current crash streak began; see crashWindow
}

// nodeBusyKey marks a server this node is deliberately holding down while it
// works on the server's files (reinstall, storage migration). See isNodeBusy.
// CORE READS THIS TOO. handlers.annotateStalledInstalls asks whether the key
// exists to tell "an install nobody is working on" apart from "an install still
// running", which is how a node that died mid-install stops looking like a
// server stuck forever with no explanation. The format is therefore a
// cross-component contract with a copy on each side, pinned by a test in both -
// renaming it here alone silently disables that detection, with nothing failing.
func nodeBusyKey(uuid string) string {
	return fmt.Sprintf("dylaris:server:%s:node_busy", uuid)
}

// diskFullKey marks a server this node has STOPPED for exceeding its disk
// limit. See isDiskFull.
func diskFullKey(uuid string) string {
	return fmt.Sprintf("dylaris:server:%s:disk_full", uuid)
}

// isDiskFull reports whether the disk guard is holding this server down.
//
// Needed for the same reason as isNodeBusy: the guard's own signal was the
// status key, and Core's status watcher drains that key every 5 seconds. So the
// reconciler saw a stopped container, no protected status, desired_state still
// "online" - and started it straight back up. Observed live, ten seconds apart:
//
//	08:53:55 Disk quota full for <uuid> — stopping server
//	08:54:00 container stopped gracefully
//	08:54:10 reconciler: restarting crashed container mc_<uuid> (attempt 1/3)
//
// The limit was therefore enforced for about ten seconds every five minutes, and
// the two halves of the node fought each other indefinitely.
func isDiskFull(ctx context.Context, rdb *redis.Client, uuid string) bool {
	n, err := rdb.Exists(ctx, diskFullKey(uuid)).Result()
	return err == nil && n == 1
}

// isNodeBusy reports whether this node is mid-operation on the server.
//
// This exists because protectedStatuses below CANNOT carry that information.
// The status key it reads is a one-shot mailbox, not a state field: Core's
// status watcher GETs it and immediately DELETEs it, every 5 seconds. So any
// marker written there is gone within 5s no matter how often it is refreshed,
// and the reconciler's own Get then finds nothing and treats the server as
// unprotected. Measured during a reinstall - the key alternated between the
// written value and absent, and the reconciler restarted the container anyway.
//
// The busy key is node-local and nothing else consumes it, so it means what it
// says for as long as the node keeps it alive.
func isNodeBusy(ctx context.Context, rdb *redis.Client, uuid string) bool {
	n, err := rdb.Exists(ctx, nodeBusyKey(uuid)).Result()
	return err == nil && n == 1
}

// protectedStatuses are statuses that indicate the server is in a transitional
// state managed by core/node commands — the reconciler must not interfere.
// Kept as a cheap early-out for the brief window before Core drains the status
// key; isNodeBusy is what actually holds across a long operation.
var protectedStatuses = map[string]bool{
	"installing":    true,
	"pending_setup": true,
	"stopping":      true,
	"starting":      true,
	"suspended":     true,
	"disk_full":     true,
}

// reconcileDeletedContainers detects server data directories whose Docker container
// has been manually deleted and recreates the container. Only acts when desired_state
// is "online" and the server is not in a protected transitional status.
// pinSavedConfig binds a saved .node_config.json to the directory it was
// found in. The file sits in a server directory the node writes on the
// tenant's behalf, so its identity fields are not trusted: the server is the
// directory, and the sub-server is one name in it. A config naming another
// server's uuid, or a sub-server of "../<uuid>/x", would have rebuilt a
// container over someone else's data with this config's image and command.
// It reports false for a sub-server that is not a single name.
func pinSavedConfig(config *ServerConfig, uuid string) bool {
	config.UUID = uuid
	a := config.ActiveSubServer
	return a != "." && a != ".." && !strings.ContainsAny(a, `/\`)
}

func reconcileDeletedContainers(ctx context.Context, rdb *redis.Client, dm *DockerManager, storage *StorageManager) {
	// Build a set of UUIDs that currently have a Docker container (running or stopped).
	existing, err := dm.ListAllMCContainers()
	if err != nil {
		log.Printf("reconciler(deleted): failed to list containers: %v", err)
		return
	}
	containerUUIDs := make(map[string]bool, len(existing))
	for _, c := range existing {
		containerUUIDs[c.UUID] = true
	}

	// Scan all storage paths for server directories.
	for _, storagePath := range storage.Paths() {
		entries, err := os.ReadDir(storagePath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name() == "" || e.Name()[0] == '.' {
				continue
			}
			uuid := e.Name()

			// Skip if a container already exists for this UUID.
			if containerUUIDs[uuid] {
				continue
			}

			// Read the saved node config — if missing, we can't recreate.
			configPath := filepath.Join(storagePath, uuid, ".node_config.json")
			data, err := os.ReadFile(configPath)
			if err != nil {
				// No config file: either a non-server directory or pre-existing server.
				continue
			}

			// The same holds as the crash-restart pass (restartBlocked). The busy
			// hold matters most here: a reinstall REMOVES the container
			// (RecreateWithCommand is stop+remove+create), so during one the
			// server looks exactly like a manually deleted container, and
			// recreating it from the saved config would race the installer over
			// the same directory.
			statusKey := fmt.Sprintf("dylaris:server:%s:status", uuid)
			if restartBlocked(ctx, rdb, storage, uuid) {
				continue
			}

			// Only recreate if desired_state is "online".
			desiredKey := fmt.Sprintf("dylaris:server:%s:desired_state", uuid)
			desired, err := rdb.Get(ctx, desiredKey).Result()
			if err != nil || desired != "online" {
				continue
			}

			var config ServerConfig
			if err := json.Unmarshal(data, &config); err != nil {
				log.Printf("reconciler(deleted): bad config for %s: %v", uuid, err)
				continue
			}

			// Guard against resurrecting a server whose active sub-server
			// was just deleted. The saved config still references the
			// dead sub-server name; if we Recreate*, Docker happily
			// auto-creates the empty bind-source dir and we end up with
			// a phantom MC server in a freshly-rebuilt empty folder. The
			// fix is to require both:
			//   a) the saved active sub-server name is non-empty, AND
			//   b) the corresponding dir actually exists on disk.
			// Anything else means there's no valid sub-server to start
			// and the server should stay down until the user picks one
			// in the Setup tab. Also force status to pending_setup so
			// the panel reflects that reality.
			if !pinSavedConfig(&config, uuid) {
				log.Printf("reconciler(deleted): mc_%s config names sub-server %q, not a name; leaving stopped", uuid, config.ActiveSubServer)
				continue
			}
			if config.ActiveSubServer == "" {
				log.Printf("reconciler(deleted): mc_%s has empty active sub-server — leaving stopped, marking pending_setup", uuid)
				rdb.Set(ctx, statusKey, "pending_setup", 30*time.Second)
				continue
			}
			activeSubPath := filepath.Join(storagePath, uuid, config.ActiveSubServer)
			if st, err := os.Stat(activeSubPath); err != nil || !st.IsDir() {
				log.Printf("reconciler(deleted): mc_%s active sub-server %q missing on disk — leaving stopped, marking pending_setup", uuid, config.ActiveSubServer)
				rdb.Set(ctx, statusKey, "pending_setup", 30*time.Second)
				continue
			}

			log.Printf("reconciler(deleted): container mc_%s missing — recreating from saved config", uuid)

			// RecreateWithCommand handles stop+remove (no-op if missing), port binding, and start.
			// It errors only if network or container create fails.
			if err := dm.RecreateWithCommand(config); err != nil {
				log.Printf("reconciler(deleted): failed to recreate mc_%s: %v", uuid, err)
			} else {
				log.Printf("reconciler(deleted): mc_%s recreated and started", uuid)
				rdb.Set(ctx, statusKey, "starting", 30*time.Second)
			}
		}
	}
}

// restartBlocked reports whether the reconciler must leave a stopped container
// alone although its desired state is "online".
func restartBlocked(ctx context.Context, rdb *redis.Client, storage *StorageManager, uuid string) bool {
	// A protected status.
	statusKey := fmt.Sprintf("dylaris:server:%s:status", uuid)
	if status, err := rdb.Get(ctx, statusKey).Result(); err == nil && protectedStatuses[status] {
		return true
	}
	// An operation this node is running right now, which the status key cannot
	// tell us (see isNodeBusy).
	if isNodeBusy(ctx, rdb, uuid) {
		return true
	}
	// A server the disk guard deliberately stopped. Restarting it would put a
	// server that is over its limit straight back over it.
	if isDiskFull(ctx, rdb, uuid) {
		return true
	}
	// A server this node staged a move of. Once Core sets it online for its NEW
	// node, the desired state reads "online" here too, and this restarted the
	// stopped source container on data the move's cleanup deletes moments
	// later - and left it running for good.
	return migrationInFlight(storage, uuid)
}

// StartReconciler runs a periodic loop that compares actual Docker container
// state against the desired state stored in Redis and auto-restarts crashed
// containers when desired_state is "online".
func StartReconciler(ctx context.Context, rdb *redis.Client, dm *DockerManager, storage *StorageManager) {
	log.Println("Reconciler started (interval: 15s)")
	sweepStaleMigrationArchives(storage)
	tracker := make(map[string]*reconcileInfo)
	var mu sync.Mutex

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	reconcile := func() {
		// Pass 1: restart crashed containers (container exists but not running).
		// Pass 2: recreate manually deleted containers (container missing entirely).
		reconcileDeletedContainers(ctx, rdb, dm, storage)

		containers, err := dm.ListAllMCContainers()
		if err != nil {
			log.Printf("reconciler: failed to list containers: %v", err)
			return
		}

		mu.Lock()
		defer mu.Unlock()

		for _, c := range containers {
			desiredKey := fmt.Sprintf("dylaris:server:%s:desired_state", c.UUID)
			desired, err := rdb.Get(ctx, desiredKey).Result()
			if err != nil {
				// Key missing — no desired state published by core yet, skip
				continue
			}

			// Only act when desired state is "online" and container is not running
			if desired != "online" {
				// If desired is "stopped", clean up tracker
				delete(tracker, c.UUID)
				continue
			}

			if c.State == "running" {
				// Container is running as desired. The streak is NOT cleared here
				// on a duration: a container that survives a minute each time and
				// dies again is looping, and clearing on "it ran for a while" is
				// exactly what let that loop run forever. It clears when the streak
				// itself ages out of the window, below.
				if info, exists := tracker[c.UUID]; exists {
					info.lastSeenAlive = time.Now()
				}
				continue
			}

			// Container is NOT running but desired_state is "online".
			if restartBlocked(ctx, rdb, storage, c.UUID) {
				continue
			}
			statusKey := fmt.Sprintf("dylaris:server:%s:status", c.UUID)

			// Initialize tracker if needed
			info, exists := tracker[c.UUID]
			if !exists {
				info = &reconcileInfo{}
				tracker[c.UUID] = info
			}

			// Age the streak out: a crash more than a window after this streak
			// began is a separate incident, so a server that dies once in a while
			// is always restarted however often it has died before.
			if info.crashCount > 0 && time.Since(info.streakStart) > crashWindow {
				info.crashCount = 0
			}
			if info.crashCount == 0 {
				info.streakStart = time.Now()
			}

			// Check if max retries exceeded
			if info.crashCount >= maxCrashRetries {
				// Set failed key so core can surface it (status_watcher's
				// consumeReconcileFailures lands it on the server row).
				failedKey := fmt.Sprintf("dylaris:server:%s:reconcile_failed", c.UUID)
				rdb.Set(ctx, failedKey, fmt.Sprintf("Container crashed %d times within %s, auto-restart disabled", info.crashCount, crashWindow), 0)
				continue
			}

			// Check backoff
			backoffIdx := info.crashCount
			if backoffIdx >= len(backoffDurations) {
				backoffIdx = len(backoffDurations) - 1
			}
			if time.Since(info.lastRestart) < backoffDurations[backoffIdx] {
				continue
			}

			// Restart the container
			log.Printf("reconciler: restarting crashed container mc_%s (attempt %d/%d)", c.UUID, info.crashCount+1, maxCrashRetries)

			// Signal "restarting" status
			rdb.Set(ctx, statusKey, "restarting", 30*time.Second)

			if err := dm.PowerAction(c.UUID, "start"); err != nil {
				log.Printf("reconciler: failed to restart mc_%s: %v", c.UUID, err)
				info.crashCount++
				info.lastRestart = time.Now()
				continue
			}

			info.crashCount++
			info.lastRestart = time.Now()

			// Clear reconcile_failed key if it was set from a previous cycle
			failedKey := fmt.Sprintf("dylaris:server:%s:reconcile_failed", c.UUID)
			rdb.Del(ctx, failedKey)
		}
	}

	// Run immediately on startup
	reconcile()

	for {
		select {
		case <-ctx.Done():
			log.Println("Reconciler stopped")
			return
		case <-ticker.C:
			reconcile()
		}
	}
}
