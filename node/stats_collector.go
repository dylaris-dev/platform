package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-pkg/validate"
)

// StatsPayload is the JSON published to Redis for each stats tick.
type StatsPayload struct {
	Timestamp  int64   `json:"ts"`
	CPU        float64 `json:"cpu"`
	CPULimit   float64 `json:"cpuLimit"`
	MemUsedMB  int64   `json:"memUsed"`
	MemLimitMB int64   `json:"memLimit"`
	// JavaHeapUsedMB is the post-GC live-heap size in MB, populated by
	// the log-shipper from `-Xlog:gc` summary lines. Omitted when no
	// GC has been observed yet (early startup, Java 8 image, etc.) so
	// the panel can fall back to the container metric without inferring
	// a misleading zero.
	JavaHeapUsedMB int64  `json:"javaHeapUsed,omitempty"`
	Players        int    `json:"players"`
	MaxPlayers     int    `json:"maxPlayers"`
	MOTD           string `json:"motd"`
}

// DiskUsagePayload is stored per-server in Redis.
type DiskUsagePayload struct {
	Total      int64            `json:"total"`
	Limit      int64            `json:"limit"`
	SubServers map[string]int64 `json:"subServers"`
	Warning    string           `json:"warning,omitempty"` // "", "80", "90", "full"
	// Enforceable reports whether the FILESYSTEM holds this limit, via project
	// quotas, or whether the platform has to hold it in software. Quotas need xfs
	// or ext4; NFS, CIFS and a Docker Desktop bind mount from a Windows host have
	// none.
	//
	// False does NOT mean the limit is ignored. disk_guard.go still measures with
	// du, still stops the server when it reaches the limit, and Core still refuses
	// to start it again until usage drops. What is lost is the filesystem's own
	// hard stop, so a server can overshoot between two scans - those run on
	// diskFallbackInterval, minutes apart, because a du walk is itself expensive.
	//
	// That distinction is the whole reason the flag exists: the owner needs to
	// know why usage can read above the limit and why the stop arrives a few
	// minutes late, not to be told the number is meaningless.
	Enforceable bool `json:"enforceable"`
}

const (
	containerScanInterval = 10 * time.Second
	liveInterval          = 2 * time.Second
	pingInterval          = 15 * time.Second
	diskInterval          = 10 * time.Second
	diskFallbackInterval  = 5 * time.Minute
	// diskFullMarkerTTL outlives several measurement cycles, so the hold survives
	// a slow scan, but expires on its own if this node dies while holding a
	// server down - the guard re-evaluates within one cycle when it comes back,
	// so a stale marker can never strand a server whose space was freed.
	diskFullMarkerTTL    = 1 * time.Hour
	historyBatchInterval = 2 * time.Minute
	pingTimeout          = 3 * time.Second
	watchCacheDuration   = 2 * time.Second
)

// statsWriteProtectedStatuses are statuses the stats collector must NOT
// overwrite when it writes "stopped"/"restarting" for a vanished container.
// This is intentionally NARROWER than reconciler.go's package-level
// protectedStatuses (no "starting"/"suspended"): the collector only guards
// against clobbering an in-flight install/setup/shutdown or a disk-full
// hold, whereas the reconciler additionally protects transient lifecycle
// states it manages directly. Named distinctly so the difference is explicit
// rather than an accidental shadow of the same identifier.
var statsWriteProtectedStatuses = map[string]bool{
	"installing":    true,
	"pending_setup": true,
	"stopping":      true,
	"disk_full":     true,
}

// statusWriteHeld reports whether something is deliberately holding this
// server's status, so the stats collector must not write its own over it.
//
// It exists because the two places the collector writes a status did not agree.
// Both read the status key, but that key is a MAILBOX: Core's watcher drains it
// every 5 seconds, so a moment after anything posts a hold there the key is
// empty and statsWriteProtectedStatuses has nothing to match. The
// stop-tracking branch learned that and started consulting the durable markers;
// the "online" branch kept reading the drained mailbox alone. Measured, from
// Core's own log, five seconds apart:
//
//	19:51:43  stopping  -> disk_full
//	19:51:48  disk_full -> online
//
// The second line left a stopped server showing as online, which also silenced
// the power route's storage-limit gate, since that gate keys on the status.
//
// One function so the next hold added here reaches both callers. isNodeBusy is
// deliberately NOT part of it: only the stop-tracking branch makes a promise
// ("restarting") that a busy node would break.
func statusWriteHeld(ctx context.Context, rdb *redis.Client, uuid, currentStatus string) bool {
	return statsWriteProtectedStatuses[currentStatus] || isDiskFull(ctx, rdb, uuid)
}

// containerSnapshot holds the latest stats for a container (used for batching).
type containerSnapshot struct {
	mu       sync.Mutex
	uuid     string
	payload  *StatsPayload
	cpuLimit float64
}

// StartStatsCollector discovers running MC containers and collects stats.
// bufferMaxLen controls the Redis Stream MAXLEN for the per-server buffer.
// quota is the filesystem quota provider (may be nil or unavailable).
func StartStatsCollector(ctx context.Context, rdb *redis.Client, dm *DockerManager, nid string, bufferMaxLen int64, quota *QuotaSet) {
	log.Println("Stats collector started")
	go watchOOMEvents(ctx, rdb, dm)

	tracked := make(map[string]context.CancelFunc) // uuid -> cancel
	snapshots := make(map[string]*containerSnapshot)
	var mu sync.Mutex

	ticker := time.NewTicker(containerScanInterval)
	defer ticker.Stop()

	// History batch ticker: every 2 min, collect snapshots from ALL containers
	// and push a single batch to the node's Redis stream for Core to consume.
	batchKey := fmt.Sprintf("dylaris:node:%s:stats:batch", nid)
	go func() {
		batchTicker := time.NewTicker(historyBatchInterval)
		defer batchTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-batchTicker.C:
				mu.Lock()
				var items []map[string]interface{}
				for _, snap := range snapshots {
					snap.mu.Lock()
					if snap.payload != nil {
						items = append(items, map[string]interface{}{
							"uuid":         snap.uuid,
							"cpu":          snap.payload.CPU,
							"cpuLimit":     snap.cpuLimit,
							"memUsed":      snap.payload.MemUsedMB,
							"memLimit":     snap.payload.MemLimitMB,
							"javaHeapUsed": snap.payload.JavaHeapUsedMB,
							"players":      snap.payload.Players,
							"maxPlayers":   snap.payload.MaxPlayers,
						})
					}
					snap.mu.Unlock()
				}
				mu.Unlock()

				if len(items) > 0 {
					batch := map[string]interface{}{
						"ts":    time.Now().Unix(),
						"stats": items,
					}
					data, _ := json.Marshal(batch)
					rdb.XAdd(ctx, &redis.XAddArgs{
						Stream: batchKey,
						MaxLen: 100,
						Approx: true,
						Values: map[string]interface{}{"data": string(data)},
					})
				}
			}
		}
	}()

	scan := func() {
		containers, err := dm.ListRunningMCContainers()
		if err != nil {
			log.Printf("stats: list containers error: %v", err)
			return
		}

		running := make(map[string]bool)
		for _, c := range containers {
			running[c.UUID] = true
		}

		mu.Lock()
		defer mu.Unlock()

		// Stop collectors for containers that are no longer running
		for uuid, cancel := range tracked {
			if !running[uuid] {
				cancel()
				delete(tracked, uuid)
				delete(snapshots, uuid)
				// Check desired state before reporting status — but don't overwrite protected statuses
				statusKey := fmt.Sprintf("dylaris:server:%s:status", uuid)
				currentStatus, _ := rdb.Get(ctx, statusKey).Result()
				// statsWriteProtectedStatuses alone cannot answer this: it reads the
				// status key, which Core's status watcher drains every 5 seconds, so
				// during a long operation it usually reads empty and the guard passes.
				// Observed live during a reinstall - the collector wrote "restarting"
				// over the node's own "installing". That is not just a wrong label:
				// the message it stands for ("Reconciler will handle restart") is
				// false while the node is deliberately holding the reconciler off.
				// isDiskFull for the same reason as isNodeBusy: without it this
				// writes "restarting" over a server the guard is deliberately
				// holding down, promising a restart that is not coming.
				if !statusWriteHeld(ctx, rdb, uuid, currentStatus) && !isNodeBusy(ctx, rdb, uuid) {
					desiredKey := fmt.Sprintf("dylaris:server:%s:desired_state", uuid)
					desired, _ := rdb.Get(ctx, desiredKey).Result()
					if desired == "online" {
						// Reconciler will handle restart — signal "restarting" instead of "stopped"
						rdb.Set(ctx, statusKey, "restarting", 30*time.Second)
					} else {
						rdb.Set(ctx, statusKey, "stopped", 30*time.Second)
					}
				}
			}
		}

		// Start collectors for new containers
		for _, c := range containers {
			if _, ok := tracked[c.UUID]; !ok {
				cctx, cancel := context.WithCancel(ctx)
				tracked[c.UUID] = cancel
				snap := &containerSnapshot{uuid: c.UUID}
				snapshots[c.UUID] = snap
				go collectForContainer(cctx, rdb, dm, c.UUID, c.ContainerName, bufferMaxLen, quota, snap)
			}
		}
	}

	// Release sweep for servers the disk guard is HOLDING DOWN. It has to live
	// out here, not in collectForContainer, because that only runs for RUNNING
	// containers - and a held server is stopped by definition. Without this the
	// hold can never lift: the guard stops the server, Core refuses to start it
	// while its status is disk_full, and the only code that would clear the
	// marker needs the container to be running.
	//
	// Before the marker existed the reconciler restarted the server ten seconds
	// after every stop, and that bug was accidentally providing this recovery
	// path. Fixing the hold without adding this would strand a server whose
	// owner has already freed the space.
	go func() {
		t := time.NewTicker(diskFallbackInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				releaseResolvedDiskHolds(ctx, rdb, quota)
				if containers, err := dm.ListRunningMCContainers(); err == nil {
					running := make(map[string]bool, len(containers))
					for _, c := range containers {
						running[c.UUID] = true
					}
					publishStoppedDiskUsage(ctx, rdb, quota, running)
				}
			}
		}
	}()

	scan()
	for {
		select {
		case <-ctx.Done():
			mu.Lock()
			for _, cancel := range tracked {
				cancel()
			}
			mu.Unlock()
			return
		case <-ticker.C:
			scan()
		}
	}
}

func collectForContainer(ctx context.Context, rdb *redis.Client, dm *DockerManager, uuid, containerName string, bufferMaxLen int64, quota *QuotaSet, snap *containerSnapshot) {
	log.Printf("stats: tracking %s", containerName)

	liveTicker := time.NewTicker(liveInterval)
	pingTicker := time.NewTicker(pingInterval)

	// Disk interval depends on quota availability
	dInterval := diskFallbackInterval
	if quota.AnyAvailable() {
		dInterval = diskInterval
	}
	diskTicker := time.NewTicker(dInterval)

	defer liveTicker.Stop()
	defer pingTicker.Stop()
	defer diskTicker.Stop()

	liveKey := fmt.Sprintf("dylaris:server:%s:stats:live", uuid)
	bufferKey := fmt.Sprintf("dylaris:server:%s:stats:buffer", uuid)
	watchKey := fmt.Sprintf("dylaris:server:%s:stats:watching", uuid)
	statusKey := fmt.Sprintf("dylaris:server:%s:status", uuid)
	// Live heap size from the log-shipper's GC log parser. May be absent
	// (no GC yet, Java 8 image, etc.) -- we treat that as "no value" and
	// the panel falls back to the container metric.
	heapKey := fmt.Sprintf("dylaris:server:%s:java-heap", uuid)

	var lastPing *SLPResponse
	var pingMu sync.Mutex
	// A server that stops answering pings (hung, crashed inside a running
	// container, restarting) would otherwise keep reporting its last player
	// count forever, and Core sums those into the platform's players online.
	var pingFails int

	// Cache watching key check (avoid Redis roundtrip every 2s)
	var watchCache bool
	var watchCacheTime time.Time

	// Cache status to avoid Redis roundtrips every 2s
	var lastWrittenStatus string
	var statusCacheTime time.Time

	isWatching := func() bool {
		if time.Since(watchCacheTime) < watchCacheDuration {
			return watchCache
		}
		n, err := rdb.Exists(ctx, watchKey).Result()
		watchCache = err == nil && n > 0
		watchCacheTime = time.Now()
		return watchCache
	}

	// pingOnce resolves mc_<uuid>'s address from the Docker daemon (guarded to a
	// private IP) and SLP-pings it. Resolving via the daemon rather than dialling
	// the container NAME is what makes this work on a host-net node (whose resolver
	// is the host's, not Docker's) and removes the DNS-wildcard target the RCON/
	// tab-proxy paths already dropped. Uses the admin-configurable global container
	// port (default 25565) so the target stays in sync with MC's listen port.
	pingOnce := func() (*SLPResponse, error) {
		addr, err := resolveMCAddr(uuid, getContainerPort())
		if err != nil {
			return nil, err
		}
		return PingMinecraftServer(addr, pingTimeout)
	}

	// Initial ping.
	go func() {
		resp, err := pingOnce()
		if err == nil {
			pingMu.Lock()
			lastPing = resp
			pingMu.Unlock()
		}
	}()

	// Initial disk scan
	go func() {
		measuredFrom := time.Now()
		if usage := getDiskUsage(ctx, rdb, uuid, quota); usage != nil {
			publishDiskUsage(ctx, rdb, uuid, usage, measuredFrom, 10*time.Minute)
		}
	}()

	getCPULimit := func() float64 {
		info, err := dm.cli.ContainerInspect(dm.ctx, containerName)
		if err != nil {
			return 0
		}
		if info.HostConfig.NanoCPUs > 0 {
			return float64(info.HostConfig.NanoCPUs) / 1e9
		}
		return 0
	}

	cpuLimit := getCPULimit()
	snap.mu.Lock()
	snap.cpuLimit = cpuLimit
	snap.mu.Unlock()

	var prevCPU *PrevCPUStats
	var memGuardState memGuard
	collectAndPublish := func() {
		stats, newPrev, err := dm.GetContainerStats(containerName, prevCPU)
		prevCPU = newPrev
		if err != nil || stats == nil {
			return
		}

		pingMu.Lock()
		ping := lastPing
		pingMu.Unlock()

		// Scale CPU% relative to allocated cores (100% = fully using allocated CPU)
		cpuPct := stats.CPUPercent
		if cpuLimit > 0 {
			cpuPct = cpuPct / cpuLimit
		}

		payload := StatsPayload{
			Timestamp:  time.Now().Unix(),
			CPU:        cpuPct,
			CPULimit:   cpuLimit,
			MemUsedMB:  stats.MemUsedMB,
			MemLimitMB: stats.MemLimitMB,
		}
		// Pull the latest post-GC heap size if the log-shipper has seen
		// one. Best-effort: a missing/unparseable key just means we omit
		// the field for this tick and the panel keeps using the previous
		// value (chart smoothing) or falls back to container memory.
		if heapStr, err := rdb.Get(ctx, heapKey).Result(); err == nil {
			if mb, perr := strconv.ParseInt(heapStr, 10, 64); perr == nil && mb > 0 {
				payload.JavaHeapUsedMB = mb
			}
		}
		if ping != nil {
			payload.Players = ping.Players
			payload.MaxPlayers = ping.MaxPlayers
			payload.MOTD = ping.MOTD
		}

		applyMemGuard(ctx, rdb, uuid, &memGuardState, stats.GuardMemMB, stats.MemLimitMB)

		data, _ := json.Marshal(payload)

		// Update snapshot for batch collection
		snap.mu.Lock()
		snap.payload = &payload
		snap.mu.Unlock()

		// Always write to buffer stream (replaces sorted set)
		rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: bufferKey,
			MaxLen: bufferMaxLen,
			Approx: true,
			Values: map[string]interface{}{"data": string(data)},
		})

		// Only publish to pub/sub if someone is watching (on-demand)
		if isWatching() {
			rdb.Publish(ctx, liveKey, string(data))
		}

		// Container is running and collecting stats → online
		newStatus := "online"
		if newStatus != lastWrittenStatus || time.Since(statusCacheTime) > 15*time.Second {
			// The status key alone cannot answer this - see statusWriteHeld.
			currentStatus, _ := rdb.Get(ctx, statusKey).Result()
			if !statusWriteHeld(ctx, rdb, uuid, currentStatus) {
				rdb.Set(ctx, statusKey, newStatus, 30*time.Second)
				lastWrittenStatus = newStatus
			}
			statusCacheTime = time.Now()
		}
	}

	for {
		select {
		case <-ctx.Done():
			log.Printf("stats: stopped tracking %s", containerName)
			return
		case <-liveTicker.C:
			collectAndPublish()
		case <-pingTicker.C:
			go func() {
				resp, err := pingOnce()
				pingMu.Lock()
				lastPing, pingFails = nextPingState(lastPing, pingFails, resp, err)
				pingMu.Unlock()
			}()
		case <-diskTicker.C:
			go func() {
				measuredFrom := time.Now()
				usage := getDiskUsage(ctx, rdb, uuid, quota)
				if usage != nil {
					publishDiskUsage(ctx, rdb, uuid, usage, measuredFrom, 10*time.Minute)

					if usage.Warning == "full" {
						// The marker, not the status key, decides whether this server is
						// already being held down. The status key is a mailbox Core drains
						// every 5s, so reading it here answered "not stopped yet" on nearly
						// every pass - and the reconciler, reading the same drained key,
						// started the server again ten seconds after each stop.
						if !isDiskFull(ctx, rdb, uuid) {
							log.Printf("Disk quota full for %s — stopping server", uuid)
							// Set BEFORE the stop: gracefulStop takes seconds and the
							// reconciler ticks during them.
							rdb.Set(ctx, diskFullKey(uuid), "1", diskFullMarkerTTL)
							gracefulStop(rdb, uuid, dm)
							rdb.Set(ctx, fmt.Sprintf("dylaris:server:%s:status", uuid), "disk_full", 30*time.Second)
							// Publish disk_full event so panel reacts immediately
							evt, _ := json.Marshal(map[string]interface{}{
								"type":    "disk_warning",
								"warning": "disk_full",
								"total":   usage.Total,
								"limit":   usage.Limit,
							})
							rdb.Publish(ctx, liveKey, string(evt))
						}
					} else {
						// Push disk warning via live channel so panel reacts immediately
						if usage.Warning != "" {
							evt, _ := json.Marshal(map[string]interface{}{
								"type":    "disk_warning",
								"warning": usage.Warning,
								"total":   usage.Total,
								"limit":   usage.Limit,
							})
							rdb.Publish(ctx, liveKey, string(evt))
						}

						// Auto-resolve: usage is back under the limit, so release the hold.
						// Keyed on the marker for the same reason the trip is: the status
						// key this used to read is drained by Core, so the branch almost
						// never fired and a freed-up server could stay held.
						if isDiskFull(ctx, rdb, uuid) {
							log.Printf("Disk quota resolved for %s — releasing the hold, setting status to stopped", uuid)
							rdb.Del(ctx, diskFullKey(uuid))
							rdb.Set(ctx, fmt.Sprintf("dylaris:server:%s:status", uuid), "stopped", 30*time.Second)
							evt, _ := json.Marshal(map[string]interface{}{
								"type":    "disk_warning",
								"warning": "resolved",
								"total":   usage.Total,
								"limit":   usage.Limit,
							})
							rdb.Publish(ctx, liveKey, string(evt))
						}
					}
				}
			}()
		}
	}
}

// getDiskUsage returns disk usage, from the quota system where the filesystem
// supports it and from a du scan where it does not.
//
// The du path matters twice over: without it a server on NFS/CIFS reported NO
// disk usage at all (this function simply returned nil), and it is the only
// measurement the disk guard has to work with there.
func getDiskUsage(ctx context.Context, rdb *redis.Client, uuid string, quota *QuotaSet) *DiskUsagePayload {
	var usage *DiskUsagePayload
	enforceable := quota != nil && quota.IsAvailableFor(uuid)
	if enforceable {
		usage = quota.GetDiskUsage(uuid)
	}
	if usage == nil {
		usage = duDiskUsage(ctx, rdb, uuid)
	}
	if usage == nil {
		return nil
	}
	// Same question the quota lookup above already answered: can this server's
	// filesystem hold a limit, or is the number only recorded? A quota read that
	// failed and fell through to du is not enforceable either, so this is set
	// from the answer rather than from which branch produced the payload.
	usage.Enforceable = enforceable

	// Set warning level based on limit utilization
	if usage.Limit > 0 && usage.Total > 0 {
		pct := float64(usage.Total) / float64(usage.Limit)
		switch {
		case pct >= 1.0:
			usage.Warning = "full"
		case pct >= 0.9:
			usage.Warning = "90"
		case pct >= 0.8:
			usage.Warning = "80"
		}
	}

	return usage
}

// duDiskUsage measures a server directory with du, for filesystems that cannot
// do project quotas. Deliberately not called on quota-capable paths: du walks
// every inode, which is exactly what quotas exist to avoid.
func duDiskUsage(ctx context.Context, rdb *redis.Client, uuid string) *DiskUsagePayload {
	if globalStorageMgr == nil {
		return nil
	}
	dir := globalStorageMgr.GetServerDir(uuid)
	if dir == "" {
		return nil
	}
	total := dirSize(dir)
	if total < 0 {
		return nil
	}
	return &DiskUsagePayload{
		Total: total,
		Limit: loadDiskLimit(ctx, rdb, uuid) * 1024 * 1024,
		// The quota path fills this and this one did not, which made it the
		// only path most installs ever take with the map missing: project
		// quotas need xfs or ext4, so NFS, CIFS and every Docker Desktop bind
		// mount land here. Core reads the map to enforce the per-server
		// sub-server limit, and len(nil) is never >= a positive limit, so that
		// setting silently did nothing wherever quotas were unavailable.
		//
		// The extra du walks cost little here: the parent walk above has just
		// pulled the same inodes through the cache, and this path runs on
		// diskFallbackInterval, minutes apart.
		SubServers: scanSubServerSizes(dir),
	}
}

// scanSubServerSizes measures each sub-server directory under a server root.
//
// Both disk-usage paths report the same map, so they build it the same way
// rather than each doing its own readdir: a breakdown that means one thing
// under quotas and another without them is worse than no breakdown.
// Dot-directories are skipped - .dylaris-backups and friends are not
// sub-servers, and counting them would inflate the limit Core enforces.
func scanSubServerSizes(serverDir string) map[string]int64 {
	sizes := make(map[string]int64)
	entries, err := os.ReadDir(serverDir)
	if err != nil {
		return sizes
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			sizes[e.Name()] = dirSize(filepath.Join(serverDir, e.Name()))
		}
	}
	return sizes
}

// dirSize returns the total size of a directory in bytes.
func dirSize(path string) int64 {
	out, err := exec.Command("du", "-sb", path).Output()
	if err == nil {
		fields := strings.Fields(string(out))
		if len(fields) > 0 {
			if size, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
				return size
			}
		}
	}

	// Fallback: manual walk
	var size int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
}

// stoppedDiskUsageTTL outlives two sweeps, so one slow sweep does not leave a
// gap in which an upload finds no usage and goes through unchecked.
const stoppedDiskUsageTTL = 3 * diskFallbackInterval

// publishStoppedDiskUsage measures every server on this node whose container is
// not running, and publishes it where a running server's collector does.
//
// Usage was only ever measured for RUNNING containers, and the key expires ten
// minutes after the last measurement. Every upload check - the panel's, Beam's
// and SFTP's - reads that key and lets the upload through when it is missing,
// so a stopped server's owner could fill the node's disk for every tenant on
// it. Stopped is exactly when files get uploaded.
func publishStoppedDiskUsage(ctx context.Context, rdb *redis.Client, quota *QuotaSet, running map[string]bool) {
	if rdb == nil || globalStorageMgr == nil {
		return
	}
	for _, uuid := range localServerUUIDs() {
		if running[uuid] {
			continue
		}
		measuredFrom := time.Now()
		usage := getDiskUsage(ctx, rdb, uuid, quota)
		if usage == nil {
			continue
		}
		publishDiskUsage(ctx, rdb, uuid, usage, measuredFrom, stoppedDiskUsageTTL)
	}
}

// looksLikeServerUUID keeps the sweeps to server directories: a canonical
// 8-4-4-4-12 UUID, optionally followed by "_<suffix>". It used to demand the
// bare UUID, and the panel mints "<ownerUUID>_<random>" - so every real server
// was skipped: a stopped server never had its disk usage published at all.
// Stricter than validate.IsServerUUID, which only rules out injection and
// would take any directory name.
func looksLikeServerUUID(s string) bool {
	if len(s) > 37 && s[36] == '_' {
		if !validate.IsServerUUID(s) {
			return false
		}
		s = s[:36]
	}
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}

// releaseResolvedDiskHolds lifts the disk guard's hold on any server this node
// hosts whose usage is back under its limit.
//
// Separate from the per-container measurement on purpose: that one is started
// per RUNNING container, and a server the guard is holding is stopped. The two
// writes mirror the trip - drop the marker so the reconciler may start the
// server again, and report "stopped" so Core stops refusing a manual start.
func releaseResolvedDiskHolds(ctx context.Context, rdb *redis.Client, quota *QuotaSet) {
	if rdb == nil || globalStorageMgr == nil {
		return
	}
	// This node's own servers, from its storage paths, and not a walk over
	// dylaris:server:*:disk_full: the node is losing SCAN, which lists every
	// key NAME on the platform whatever its ACL's patterns say.
	for _, uuid := range localServerUUIDs() {
		if n, err := rdb.Exists(ctx, diskFullKey(uuid)).Result(); err != nil || n == 0 {
			continue
		}
		usage := getDiskUsage(ctx, rdb, uuid, quota)
		if usage == nil || usage.Limit <= 0 {
			continue
		}
		if usage.Total >= usage.Limit {
			continue // still over: keep holding
		}
		log.Printf("Disk quota resolved for %s while stopped — releasing the hold", uuid)
		rdb.Del(ctx, diskFullKey(uuid))
		rdb.Set(ctx, fmt.Sprintf("dylaris:server:%s:status", uuid), "stopped", 30*time.Second)
	}
}

// localServerUUIDs are the servers whose directory lives on this node, where
// the storage manager places it.
func localServerUUIDs() []string {
	if globalStorageMgr == nil {
		return nil
	}
	var out []string
	for _, base := range globalStorageMgr.Paths() {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			uuid := e.Name()
			if !e.IsDir() || !looksLikeServerUUID(uuid) {
				continue
			}
			// Only where this server actually lives: a copy left on another
			// path is not it.
			if globalStorageMgr.GetServerDir(uuid) != filepath.Join(base, uuid) {
				continue
			}
			out = append(out, uuid)
		}
	}
	return out
}

// pingFailsToForget is how many pings in a row may fail before the last answer
// stops counting: one slow ping under load is not a dead server.
const pingFailsToForget = 3

// nextPingState keeps the last good ping answer through a few failures, then
// drops it so the server reports no players instead of stale ones.
func nextPingState(last *SLPResponse, fails int, resp *SLPResponse, err error) (*SLPResponse, int) {
	if err == nil {
		return resp, 0
	}
	fails++
	if fails >= pingFailsToForget {
		return nil, fails
	}
	return last, fails
}
