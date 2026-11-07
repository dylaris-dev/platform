package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/redis/go-redis/v9"
)

// The disk-full machinery (measure -> Warning "full" -> graceful stop ->
// status disk_full -> Core refuses start -> auto-resolve when usage drops) has
// existed for quota-capable filesystems all along. It simply never RAN on
// NFS/CIFS, because the measurement came exclusively from project quotas and
// those do not exist there: getDiskUsage returned nil, so no usage was published
// and no limit was ever compared.
//
// The fix is the du fallback in stats_collector.go. It needs one thing the quota
// path got for free - the server's limit, which the quota system stored itself.
// That is all this file is: a cache of the limit Core last pushed, so the du
// path can fill DiskUsagePayload.Limit and the existing threshold logic applies
// unchanged on filesystems that cannot enforce anything themselves.
//
// Consequence worth knowing: on those filesystems this is a GUARD, not a limit.
// A du scan over a large server directory on a network share is slow and is
// itself I/O, so it runs on diskFallbackInterval (5 min). A server can overshoot
// between two scans. Stopping is nonetheless safe here, because only the
// SERVER's limit was reached, not the disk - there is still room for the
// shutdown save to succeed.

// diskLimitKeyFmt caches the limit Core last pushed for a server. Core sends it
// with create/update commands; without caching it, the du path would have no
// limit to compare against between two commands.
const diskLimitKeyFmt = "node:%s:server:%s:disk_limit_mb"

func diskLimitKey(uuid string) string { return fmt.Sprintf(diskLimitKeyFmt, nodeID, uuid) }

// recordDiskLimit caches the limit Core pushed. No TTL: it has to outlive the
// gap between two commands, and it is replaced on every update.
func recordDiskLimit(ctx context.Context, rdb *redis.Client, uuid string, limitMB int64) {
	if rdb == nil || uuid == "" {
		return
	}
	if limitMB <= 0 {
		// Unlimited: drop the cached value so the du path reports no limit
		// rather than keeping a stale one that would still stop the server.
		rdb.Del(ctx, diskLimitKey(uuid))
		return
	}
	rdb.Set(ctx, diskLimitKey(uuid), limitMB, 0)
}

// applyDiskLimit puts a server's directory under the quota of the filesystem it
// lives on NOW and enforces limitMB there. A server that moved - to this node,
// or to another storage path on it - arrived on a filesystem whose quota never
// heard of it, and ran with no disk limit until someone changed its resources.
func applyDiskLimit(ctx context.Context, rdb *redis.Client, quota *QuotaSet, uuid string, limitMB int64) {
	if quota == nil || uuid == "" {
		return
	}
	recordDiskLimit(ctx, rdb, uuid, limitMB)
	if limitMB <= 0 {
		if err := quota.SetLimit(uuid, 0); err != nil {
			log.Printf("quota: limit %s: %v", uuid, err)
		}
	}
	if err := countDiskUsage(ctx, rdb, quota, uuid, limitMB, false); err != nil {
		log.Printf("quota: assign %s: %v", uuid, err)
	}
}

// diskGraceKeyFmt marks a server that went over its limit only because its
// existing data was counted for the first time (owner decision 2026-11-05:
// warn, do not stop). The value is diskGrace: the limit it was granted under
// and the hard limit the filesystem holds instead while it lasts, in bytes.
const diskGraceKeyFmt = "node:%s:server:%s:disk_grace"

func diskGraceKey(uuid string) string { return fmt.Sprintf(diskGraceKeyFmt, nodeID, uuid) }

// diskCountedKeyFmt marks a server whose whole tree has been counted under its
// project on this node. The boot recount skips it from then on, which is what
// makes the grace once per server and keeps a restart from walking every tree.
const diskCountedKeyFmt = "node:%s:server:%s:disk_counted"

func diskCountedKey(uuid string) string { return fmt.Sprintf(diskCountedKeyFmt, nodeID, uuid) }

// diskLiftedKeyFmt marks a server whose hard limit a recount took off and has
// not put back yet. A node killed during the tag left the limit off for good:
// the boot recount skips a counted server, and one limited before the cache
// existed read the lifted 0 off its project as its limit.
const diskLiftedKeyFmt = "node:%s:server:%s:disk_lifted"

func diskLiftedKey(uuid string) string { return fmt.Sprintf(diskLiftedKeyFmt, nodeID, uuid) }

// diskLiftedNoGrace is the lifted marker of a recount that may not grant the
// grace (a move, a restore, a resize). Any other value is what every recount
// wrote before, read as the grace-eligible boot recount it may have been.
const diskLiftedNoGrace = "nograce"

type diskGrace struct{ limit, ceiling int64 }

// holds reports whether the grace still applies: same limit, still over it,
// and short of the ceiling. At the ceiling the filesystem refuses writes again,
// so the guard's stop is the better outcome than a server failing silently.
func (g diskGrace) holds(limit, total int64) bool {
	return limit > 0 && g.limit == limit && total >= limit && total < g.ceiling
}

// diskGraceHeadroom is how far past usage the hard limit is lifted while a
// grace lasts: room for world saves, not for growth without end.
func diskGraceHeadroom(limit int64) int64 {
	return max(limit/10, 1<<30)
}

func loadDiskGrace(ctx context.Context, rdb *redis.Client, uuid string) (diskGrace, bool) {
	g, ok, _ := readDiskGrace(ctx, rdb, uuid)
	return g, ok
}

// readDiskGrace is loadDiskGrace that tells a server with no grace apart from
// a Redis that could not say: err is set only for the latter.
func readDiskGrace(ctx context.Context, rdb *redis.Client, uuid string) (diskGrace, bool, error) {
	if rdb == nil {
		return diskGrace{}, false, nil
	}
	v, err := rdb.Get(ctx, diskGraceKey(uuid)).Result()
	if errors.Is(err, redis.Nil) {
		return diskGrace{}, false, nil
	}
	if err != nil {
		return diskGrace{}, false, err
	}
	g, ok := parseDiskGrace(v)
	return g, ok, nil
}

func parseDiskGrace(v string) (diskGrace, bool) {
	var g diskGrace
	if _, err := fmt.Sscan(v, &g.limit, &g.ceiling); err != nil {
		return diskGrace{}, false
	}
	return g, true
}

// adoptMovedDiskGrace takes over the grace a moved server held on its source
// node, as Core handed it over. The key is node-scoped and the source forgets
// it on cleanup, so a graced server arrived as an ordinary over-limit one and
// was stopped. Only under the limit it was granted under; countDiskUsage
// keeps it from there while it holds.
func adoptMovedDiskGrace(ctx context.Context, rdb *redis.Client, uuid string, limitMB int64, grace string) {
	if rdb == nil || grace == "" || limitMB <= 0 {
		return
	}
	if g, ok := parseDiskGrace(grace); ok && g.limit == limitMB<<20 {
		saveDiskGrace(ctx, rdb, uuid, g)
	}
}

func saveDiskGrace(ctx context.Context, rdb *redis.Client, uuid string, g diskGrace) {
	rdb.Set(ctx, diskGraceKey(uuid), fmt.Sprintf("%d %d", g.limit, g.ceiling), 0)
}

type diskLimiter interface {
	SetLimit(uuid string, limitMB int64) error
}

type quotaAssigner interface {
	diskLimiter
	AssignQuota(uuid string) error
	GetDiskUsage(uuid string) *DiskUsagePayload
}

// assignNewServerQuota puts a new server's directory under its project and
// limit. Its tree is counted from birth, so it is marked counted: unmarked,
// its first boot recount ran with the grace allowed, and a full server
// writing while the walk lifted its limit was granted the once-only grace.
func assignNewServerQuota(ctx context.Context, rdb *redis.Client, q quotaAssigner, uuid string, limitMB int64) {
	if err := q.AssignQuota(uuid); err != nil {
		log.Printf("Quota assign warning for %s: %v", uuid, err)
	} else if rdb != nil && q.GetDiskUsage(uuid) != nil {
		rdb.Set(ctx, diskCountedKey(uuid), 1, 0)
	}
	if limitMB > 0 {
		recordDiskLimit(ctx, rdb, uuid, limitMB)
		if err := q.SetLimit(uuid, limitMB); err != nil {
			log.Printf("Quota limit warning for %s: %v", uuid, err)
		}
	}
}

// countDiskUsage tags a server's directory and everything in it with its
// project, then leaves the project's hard limit where it belongs: limitMB, or
// the ceiling while a grace holds. limitMB <= 0 leaves the hard limit alone.
//
// The hard limit is off while the tag runs. ext4 charges each re-tagged inode
// to the project through dquot_transfer, which refuses at bhard without
// CAP_SYS_RESOURCE, and no node kit grants that: a tree over its limit was
// counted up to the limit and the rest stayed uncounted (G35).
//
// allowGrace is set only by the boot recount, the one place a count can rise
// because ext4 now counts data it never counted before. A move, a restore or a
// storage move of a tree over its limit is an ordinary over-limit server.
func countDiskUsage(ctx context.Context, rdb *redis.Client, q quotaAssigner, uuid string, limitMB int64, allowGrace bool) error {
	if diskLiftHeld(uuid) {
		// A restore still extracts under its lift: putting the limit back here
		// (an update_resources beside it) failed the extraction with EDQUOT.
		// The restore's own recount applies what is cached.
		if limitMB > 0 {
			recordDiskLimit(ctx, rdb, uuid, limitMB)
		}
		log.Printf("quota: %s is restoring with its hard limit lifted; the limit goes back on when the restore ends", uuid)
		return nil
	}
	limit := limitMB << 20
	var before *DiskUsagePayload
	if allowGrace && limit > 0 {
		// Lifted but never counted is a first recount killed mid-tag: what
		// it charged is part of the tree, not what the server held before
		// the counting, and reads as no count at all.
		if rdb == nil || rdb.Exists(ctx, diskLiftedKey(uuid)).Val() == 0 {
			before = q.GetDiskUsage(uuid)
		}
		restoreLostDiskGrace(ctx, rdb, uuid, limit, before)
	}
	if limitMB > 0 {
		// Persisted before it comes off, so a node killed during the tag puts
		// it back on its next boot. A server from before the cache had the
		// limit only on the project, which reads 0 once lifted.
		if rdb != nil {
			recordDiskLimit(ctx, rdb, uuid, limitMB)
			// SetNX: an interrupted boot recount's marker stays what it was.
			var lifted any = 1
			if !allowGrace {
				lifted = diskLiftedNoGrace
			}
			// Without the marker nothing puts a lifted limit back after a
			// crash: the boot skips a counted server that has none.
			if err := rdb.SetNX(ctx, diskLiftedKey(uuid), lifted, 0).Err(); err != nil {
				// No lift and no tag, but the limit itself still goes on: an
				// update_resources is not retried, and returning here kept the
				// old limit on the filesystem. A grace that cannot be read
				// leaves the limit as it is rather than block a graced server.
				if g, ok, gerr := readDiskGrace(ctx, rdb, uuid); gerr == nil {
					enforceMB := limitMB
					if ok && g.limit == limit {
						enforceMB = g.ceiling >> 20
					}
					if serr := q.SetLimit(uuid, enforceMB); serr != nil {
						log.Printf("quota: limit %s: %v", uuid, serr)
					}
				}
				return fmt.Errorf("mark the hard limit of %s lifted for the recount: %w", uuid, err)
			}
		}
		if err := q.SetLimit(uuid, 0); err != nil {
			log.Printf("quota: lift the limit of %s for the recount: %v", uuid, err)
		}
	}
	err := q.AssignQuota(uuid)
	after := q.GetDiskUsage(uuid)
	enforceMB := limitMB
	graceUnknown := false
	if rdb != nil {
		g, ok, gerr := readDiskGrace(ctx, rdb, uuid)
		if gerr != nil {
			// Neither the real limit nor a grant: under a graced server's
			// usage the real one had the filesystem refuse its writes while
			// the guard, reading the grace fine next tick, let it run. The
			// limit stays lifted and the marker set, and the server is not
			// marked counted, so the next boot decides it.
			graceUnknown = true
			log.Printf("quota: read the disk grace of %s: %v - leaving its hard limit lifted until a recount can read it", uuid, gerr)
		} else if ok {
			// A count that cannot be read is not the server back under its
			// limit: the real limit with the grace key left in place had the
			// guard let a server run that the filesystem no longer let write.
			if g.limit == limit && (after == nil || g.holds(limit, after.Total)) {
				enforceMB = g.ceiling >> 20
			} else {
				rdb.Del(ctx, diskGraceKey(uuid))
			}
		} else if after != nil && allowGrace && limit > 0 && after.Total >= limit && (before == nil || before.Total < limit) {
			// nil before with a working quota read after: the project was not
			// in the report yet, which is a count of zero.
			enforceMB = (after.Total + diskGraceHeadroom(limit) + 1<<20 - 1) >> 20
			saveDiskGrace(ctx, rdb, uuid, diskGrace{limit: limit, ceiling: enforceMB << 20})
			log.Printf("quota: %s is over its limit now that its existing data is counted (%d of %d bytes) - warning, not stopping, with the hard limit lifted to %d MB until the limit changes or usage drops below it", uuid, after.Total, limit, enforceMB)
		}
		if after != nil && !graceUnknown {
			rdb.Set(ctx, diskCountedKey(uuid), 1, 0)
		}
		if limitMB <= 0 && !graceUnknown {
			// No hard limit is what an unlimited server should have, so a
			// marker from an earlier recount has nothing left to put back;
			// kept, it had every boot walk the tree again.
			rdb.Del(ctx, diskLiftedKey(uuid))
		}
	}
	if limitMB > 0 && !graceUnknown {
		// Asked again here: the walk takes minutes, and a sub-server restore
		// that lifted the limit meanwhile was failed with EDQUOT by the limit
		// put back mid-extraction. And once more after the SetLimit, for a lift
		// counted while it ran: liftDiskLimit counts itself before it lifts,
		// so a lift neither check saw lifts after this. No lock across the
		// SetLimit, which waits on a storage move of the server.
		if diskLiftHeld(uuid) {
			log.Printf("quota: %s started restoring during the recount; the limit goes back on when the restore ends", uuid)
		} else if serr := q.SetLimit(uuid, enforceMB); serr != nil {
			log.Printf("quota: limit %s: %v", uuid, serr)
		} else if diskLiftHeld(uuid) {
			if serr := q.SetLimit(uuid, 0); serr != nil {
				log.Printf("quota: lift the limit of %s again for its restore: %v", uuid, serr)
			}
		} else if rdb != nil {
			rdb.Del(ctx, diskLiftedKey(uuid))
		}
	}
	return err
}

// heldDiskLifts counts the lifts liftDiskLimit granted that their work has not
// ended yet. In memory on purpose: a node killed meanwhile has the marker put
// the limit back on its next boot.
var heldDiskLifts = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

func diskLiftHeld(uuid string) bool {
	heldDiskLifts.Lock()
	defer heldDiskLifts.Unlock()
	return heldDiskLifts.n[uuid] > 0
}

// endDiskLift releases a lift liftDiskLimit granted. Called before the
// recount that puts the limit back, which a held lift would skip.
func endDiskLift(uuid string) {
	heldDiskLifts.Lock()
	defer heldDiskLifts.Unlock()
	if heldDiskLifts.n[uuid] <= 1 {
		delete(heldDiskLifts.n, uuid)
		return
	}
	heldDiskLifts.n[uuid]--
}

// liftDiskLimit takes a server's hard limit off for work that holds an old and
// a new copy of its data under its project at once (a sub-server restore
// extracts beside the sub-server it replaces). It sets the same marker a
// recount does, so a node killed meanwhile puts the limit back on its next
// boot; otherwise a countDiskUsage after endDiskLift puts it back. Reports
// whether it lifted; a true has to be ended with endDiskLift.
func liftDiskLimit(ctx context.Context, rdb *redis.Client, q quotaAssigner, uuid string) bool {
	limitMB := knownDiskLimitMB(ctx, rdb, q, uuid)
	if limitMB <= 0 || rdb == nil {
		return false
	}
	recordDiskLimit(ctx, rdb, uuid, limitMB)
	// Counted before the marker and the lift, not after: a recount running
	// beside this checks the count after its own SetLimit, and one that put
	// the limit back and dropped the marker between them undid this lift.
	heldDiskLifts.Lock()
	heldDiskLifts.n[uuid]++
	heldDiskLifts.Unlock()
	if err := rdb.SetNX(ctx, diskLiftedKey(uuid), diskLiftedNoGrace, 0).Err(); err != nil {
		log.Printf("quota: mark the hard limit of %s lifted: %v - leaving it on", uuid, err)
		endDiskLift(uuid)
		return false
	}
	if err := q.SetLimit(uuid, 0); err != nil {
		log.Printf("quota: lift the limit of %s: %v", uuid, err)
	}
	return true
}

// restoreLostDiskGrace puts back a grace whose key Redis lost - it keeps it
// in memory only. A project held above the server's limit by a server still
// over it is that grace's ceiling: restored, not granted a second time.
func restoreLostDiskGrace(ctx context.Context, rdb *redis.Client, uuid string, limit int64, before *DiskUsagePayload) {
	if rdb == nil || before == nil || before.Limit <= limit || before.Total < limit || before.Total >= before.Limit {
		return
	}
	if _, ok := loadDiskGrace(ctx, rdb, uuid); !ok {
		saveDiskGrace(ctx, rdb, uuid, diskGrace{limit: limit, ceiling: before.Limit})
	}
}

// reconcileServer is the boot recount of one server, once per server on this
// node, and again for one whose limit a recount left lifted. It reports
// whether it counted.
func reconcileServer(ctx context.Context, rdb *redis.Client, q quotaAssigner, uuid string) (bool, error) {
	allowGrace := true
	if rdb != nil {
		// A read error is no answer: as "never counted" it re-walked a counted
		// server with the once-only grace allowed. Skip it until the next boot.
		n, err := rdb.Exists(ctx, diskCountedKey(uuid)).Result()
		if err != nil {
			return false, fmt.Errorf("read counted marker: %w", err)
		}
		if n == 1 {
			// An error here recounts without a grace rather than leaving a
			// limit a crash lifted off until some later boot.
			if l, err := rdb.Exists(ctx, diskLiftedKey(uuid)).Result(); err == nil && l == 0 {
				return false, nil
			}
			// Counted already: the grace stays once per server.
			allowGrace = false
		} else if v, err := rdb.Get(ctx, diskLiftedKey(uuid)).Result(); err != nil && err != redis.Nil {
			return false, fmt.Errorf("read lifted marker: %w", err)
		} else if v == diskLiftedNoGrace {
			// Lifted by a recount that could not grant it, killed mid-tag:
			// finishing it here is that recount, not the boot's first count.
			allowGrace = false
		}
	}
	return true, countDiskUsage(ctx, rdb, q, uuid, knownDiskLimitMB(ctx, rdb, q, uuid), allowGrace)
}

// knownDiskLimitMB is the limit Core last pushed, else the one on the project
// (a server limited before the cache existed). For paths Core sends no limit on.
//
// The saved config comes before the project: it survives a Redis that lost
// the cache, and while a grace holds the project's limit is its ceiling,
// which then became the server's limit for good.
func knownDiskLimitMB(ctx context.Context, rdb *redis.Client, q quotaAssigner, uuid string) int64 {
	if limitMB := loadDiskLimit(ctx, rdb, uuid); limitMB > 0 {
		return limitMB
	}
	if globalStorageMgr != nil {
		if dir := globalStorageMgr.GetServerDir(uuid); dir != "" {
			if limitMB := savedDiskLimitIn(dir); limitMB > 0 {
				return limitMB
			}
		}
	}
	if u := q.GetDiskUsage(uuid); u != nil {
		return u.Limit >> 20
	}
	return 0
}

// withGraceLimit reports a graced server against its own limit rather than the
// ceiling the filesystem holds meanwhile, so the owner sees it as full. Only
// while that is still its limit: right after a raise it read as full against
// the old one.
//
// A project with no hard limit under a known one is a lift a recount could
// not undo, not an unlimited server: reported against the known limit, the
// guard still warns and stops where the filesystem no longer does.
func withGraceLimit(ctx context.Context, rdb *redis.Client, uuid string, usage *DiskUsagePayload) {
	if usage.Limit == 0 {
		usage.Limit = loadDiskLimit(ctx, rdb, uuid) << 20
	}
	if g, ok := loadDiskGrace(ctx, rdb, uuid); ok && g.limit == loadDiskLimit(ctx, rdb, uuid)<<20 {
		usage.Limit = g.limit
	}
}

// diskGraced reports whether the disk guard must let this full server run. The
// grace lasts only while diskGrace.holds; once it ends it is gone for good and
// the server's real limit goes back on the filesystem.
func diskGraced(ctx context.Context, rdb *redis.Client, q diskLimiter, uuid string, usage *DiskUsagePayload) bool {
	if rdb == nil || usage == nil {
		return false
	}
	g, ok := loadDiskGrace(ctx, rdb, uuid)
	if !ok {
		return false
	}
	limitMB, err := rdb.Get(ctx, diskLimitKey(uuid)).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		// Unreadable is not unlimited: SetLimit(0) here took the hard limit
		// off. This tick passes; the next one reads again.
		return true
	}
	if g.limit != limitMB<<20 {
		// The limit changed, so the grace is obsolete rather than used up.
		// Whoever set the new limit recounts and puts it on the filesystem;
		// stopping here stopped the server the owner had just given more
		// disk, and a SetLimit now would land in the middle of that tag.
		rdb.Del(ctx, diskGraceKey(uuid))
		return true
	}
	if g.holds(limitMB<<20, usage.Total) {
		return true
	}
	rdb.Del(ctx, diskGraceKey(uuid))
	if q != nil {
		if err := q.SetLimit(uuid, limitMB); err != nil {
			log.Printf("quota: restore the limit of %s after its grace: %v", uuid, err)
		}
	}
	return false
}

// forgetDiskLimit drops the cached limit for a server that is going away.
// recordDiskLimit only deletes when Core pushes "unlimited", which a delete
// never does, so the delete path has to say so explicitly.
func forgetDiskLimit(ctx context.Context, rdb *redis.Client, uuid string) {
	if rdb == nil || uuid == "" {
		return
	}
	rdb.Del(ctx, diskLimitKey(uuid), diskGraceKey(uuid), diskCountedKey(uuid), diskLiftedKey(uuid))
}

// loadDiskLimit returns the cached limit in MB, or 0 for "no limit known".
func loadDiskLimit(ctx context.Context, rdb *redis.Client, uuid string) int64 {
	if rdb == nil {
		return 0
	}
	v, err := rdb.Get(ctx, diskLimitKey(uuid)).Int64()
	if err != nil {
		return 0
	}
	return v
}
