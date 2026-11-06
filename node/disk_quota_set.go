package main

import "log"

// QuotaSet holds one QuotaProvider per storage path.
//
// Project quotas are a per-FILESYSTEM facility: the project id lives in that
// filesystem's quota database and the tools are invoked against its mount point.
// A single provider bound to the first storage path therefore covered no server
// living on any other path - no usage tracking and, worse, no enforced disk
// limit, silently. That gap was mostly hidden while placement happened to prefer
// the largest disk; it became reachable in practice once an operator could pin a
// priority order across several paths.
//
// Method set mirrors QuotaProvider so call sites do not care which they hold.
type QuotaSet struct {
	storage   *StorageManager
	providers map[string]*QuotaProvider
}

// NewQuotaSet builds a provider for every configured storage path. Detection,
// tool checks and reconciliation happen per path, so one non-quota filesystem
// does not disable the others.
func NewQuotaSet(storage *StorageManager) *QuotaSet {
	qs := &QuotaSet{storage: storage, providers: map[string]*QuotaProvider{}}
	if storage == nil {
		return qs
	}
	for _, p := range storage.Paths() {
		qp := NewQuotaProvider(p)
		qp.reconcileExisting(storage.rdb, storage)
		qs.providers[p] = qp
	}
	return qs
}

// forServer returns the provider owning the filesystem the server lives on, or
// nil when the path is unknown. A nil provider makes every operation a no-op,
// which is the same behaviour as an unavailable one.
func (qs *QuotaSet) forServer(uuid string) *QuotaProvider {
	if qs == nil || qs.storage == nil {
		return nil
	}
	return qs.providers[qs.storage.GetServerPath(uuid)]
}

// AssignQuota sets up tracking for a server on its own filesystem.
func (qs *QuotaSet) AssignQuota(uuid string) error {
	qp := qs.forServer(uuid)
	if qp == nil {
		return nil
	}
	return qp.AssignQuota(uuid)
}

// SetLimit enforces the server's disk limit on its own filesystem.
func (qs *QuotaSet) SetLimit(uuid string, limitMB int64) error {
	qp := qs.forServer(uuid)
	if qp == nil {
		if limitMB > 0 {
			log.Printf("quota: cannot enforce disk limit for %s - no quota provider for its storage path", uuid)
		}
		return nil
	}
	return qp.SetLimit(uuid, limitMB)
}

// GetDiskUsage reports usage for a server, or nil when its filesystem has no
// quota support (the caller falls back to a du scan).
func (qs *QuotaSet) GetDiskUsage(uuid string) *DiskUsagePayload {
	qp := qs.forServer(uuid)
	if qp == nil {
		return nil
	}
	return qp.GetDiskUsage(uuid)
}

// RemoveQuota tears down a server's quota entries on EVERY path, not just the
// one it resolves to. Deletion runs while the server's directory and its Redis
// path key are being torn down, so path resolution is exactly the thing that is
// least trustworthy here - and removing an entry that was never there is free,
// while missing the real one leaves a stale project id behind forever.
func (qs *QuotaSet) RemoveQuota(uuid string) {
	if qs == nil {
		return
	}
	for _, qp := range qs.providers {
		if qp != nil {
			qp.RemoveQuota(uuid)
		}
	}
}

// IsAvailableFor reports whether this server's filesystem can do quotas. Ask per
// server, not per node: with mixed filesystems the answer differs per path.
func (qs *QuotaSet) IsAvailableFor(uuid string) bool {
	qp := qs.forServer(uuid)
	return qp != nil && qp.IsAvailable()
}

// AvailabilityByPath reports, per storage path, whether disk limits can actually
// be ENFORCED there. Surfaced so an operator is not left believing a limit
// applies on a filesystem that cannot carry one (NFS/CIFS report false).
func (qs *QuotaSet) AvailabilityByPath() map[string]bool {
	out := map[string]bool{}
	if qs == nil {
		return out
	}
	for path, qp := range qs.providers {
		out[path] = qp != nil && qp.IsAvailable()
	}
	return out
}
