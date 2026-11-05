package main

import (
	"context"
	"fmt"
	"log"

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
	// The limit before the tag, so the recount below compares against the
	// limit the server will actually run under.
	if err := quota.SetLimit(uuid, limitMB); err != nil {
		log.Printf("quota: limit %s: %v", uuid, err)
	}
	if err := assignQuotaCounted(ctx, rdb, quota, uuid); err != nil {
		log.Printf("quota: assign %s: %v", uuid, err)
	}
}

// diskGraceKeyFmt marks a server that went over its limit only because its
// existing data was counted for the first time (owner decision 2026-11-05:
// warn, do not stop). The value is the limit in bytes it was granted under.
const diskGraceKeyFmt = "node:%s:server:%s:disk_grace"

func diskGraceKey(uuid string) string { return fmt.Sprintf(diskGraceKeyFmt, nodeID, uuid) }

type quotaAssigner interface {
	AssignQuota(uuid string) error
	GetDiskUsage(uuid string) *DiskUsagePayload
}

// assignQuotaCounted tags a server's directory with its project and, when that
// recount is what puts it at or over its limit, grants the grace diskGraced
// honours. A server already over before the tag gets none: that is not new.
func assignQuotaCounted(ctx context.Context, rdb *redis.Client, q quotaAssigner, uuid string) error {
	before := q.GetDiskUsage(uuid)
	err := q.AssignQuota(uuid)
	after := q.GetDiskUsage(uuid)
	if rdb == nil || after == nil || after.Limit <= 0 || after.Total < after.Limit {
		return err
	}
	// nil before with a working quota read after: the project was not in the
	// report yet, which is a count of zero.
	if before == nil || before.Total < after.Limit {
		log.Printf("quota: %s is over its limit now that its existing data is counted (%d of %d bytes) - warning, not stopping, until the limit changes or usage drops below it", uuid, after.Total, after.Limit)
		rdb.Set(ctx, diskGraceKey(uuid), after.Limit, 0)
	}
	return err
}

// diskGraced reports whether the disk guard must let this full server run. The
// grace lasts only while the server is still over the limit it was granted
// under; once usage drops below it or the limit changes it is gone for good.
func diskGraced(ctx context.Context, rdb *redis.Client, uuid string, usage *DiskUsagePayload) bool {
	if rdb == nil || usage == nil {
		return false
	}
	granted, err := rdb.Get(ctx, diskGraceKey(uuid)).Int64()
	if err != nil {
		return false
	}
	if usage.Limit > 0 && usage.Total >= usage.Limit && granted == usage.Limit {
		return true
	}
	rdb.Del(ctx, diskGraceKey(uuid))
	return false
}

// forgetDiskLimit drops the cached limit for a server that is going away.
// recordDiskLimit only deletes when Core pushes "unlimited", which a delete
// never does, so the delete path has to say so explicitly.
func forgetDiskLimit(ctx context.Context, rdb *redis.Client, uuid string) {
	if rdb == nil || uuid == "" {
		return
	}
	rdb.Del(ctx, diskLimitKey(uuid), diskGraceKey(uuid))
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
