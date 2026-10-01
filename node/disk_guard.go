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
	if err := quota.AssignQuota(uuid); err != nil {
		log.Printf("quota: assign %s: %v", uuid, err)
	}
	recordDiskLimit(ctx, rdb, uuid, limitMB)
	if err := quota.SetLimit(uuid, limitMB); err != nil {
		log.Printf("quota: limit %s: %v", uuid, err)
	}
}

// forgetDiskLimit drops the cached limit for a server that is going away.
// recordDiskLimit only deletes when Core pushes "unlimited", which a delete
// never does, so the delete path has to say so explicitly.
func forgetDiskLimit(ctx context.Context, rdb *redis.Client, uuid string) {
	if rdb == nil || uuid == "" {
		return
	}
	rdb.Del(ctx, diskLimitKey(uuid))
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
