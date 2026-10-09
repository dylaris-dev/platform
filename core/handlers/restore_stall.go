package handlers

import (
	"context"

	"dylaris-core/models"

	"github.com/redis/go-redis/v9"
)

// The same hole as a stalled install, one table over.
//
// A restore is created "queued" and every SYNCHRONOUS failure moves it to
// "failed" - no queue, no storage, no push. Once the command is on the node's
// durable stream the only thing that ever moves it again is the node's result
// arriving. If the node dies while restoring, nothing writes, and the row reads
// "queued" for good.
//
// Its two closest siblings each already answer this and by different means:
// backup RUNS get a real reaper (reapAbandonedRuns, 6h, which must close the row
// because it holds storage), and the DB migration job computes staleness on READ
// rather than persisting it. Restores had neither. This takes the third
// approach, annotateStalledInstalls', because a restore is the same situation as
// a stalled install rather than the same situation as an abandoned run: nothing
// has to be released, and the work genuinely resumes.
//
// So, as there, this changes no status. The one exception is the scheduler's
// reaper (reapAbandonedRestores): after six hours with the node CONNECTED and
// not restoring, the result was lost and the row is failed. A disconnected
// node is never reaped, which is what keeps the reason below true. The node
// holds dylaris:server:<uuid>:node_busy for as long as it is restoring
// (node/main.go, holdBusyStatus on the restore path), so "is anyone working on
// this" is answerable instead of guessed, and no number can be wrong for a large
// world on a slow disk.
const restoreStallReason = "The node that would run this restore is offline. Nothing is lost - it resumes on its own when the node reconnects. If the node is gone for good, restore again once a replacement is online."

// annotateStalledRestores marks restores that cannot currently make progress.
//
// All three must hold, and each removes a false positive:
//
//   - the restore is still "queued" - anything further along has been heard from
//   - the node is NOT connected - a connected node either holds the job or is
//     handed it again by the queue, so there is nothing to report
//   - the node_busy key is absent - present means the restore is running and the
//     node simply has not finished
//
// The middle condition is what keeps the ordinary dispatch window quiet: between
// the push and the node claiming it there is no busy key yet, and reporting a
// stall there would fire on every restore anyone ever starts.
func annotateStalledRestores(ctx context.Context, rdb *redis.Client, connected func(nodeID int) bool, nodeID int, serverUUID string, restores []models.BackupRestore) {
	if connected == nil || connected(nodeID) {
		return
	}
	var candidates []int
	for i := range restores {
		if restores[i].Status == "queued" {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return
	}
	mark := func() {
		for _, i := range candidates {
			restores[i].Stalled = true
			restores[i].StallReason = restoreStallReason
		}
	}
	// Without Redis the busy key cannot be checked. Report anyway: the node is
	// known to be disconnected, which is the load-bearing half, and a stale busy
	// key from a node that is no longer there would only have hidden it.
	if rdb == nil || serverUUID == "" {
		mark()
		return
	}
	busy, err := rdb.Exists(ctx, nodeBusyKey(serverUUID)).Result()
	if err != nil {
		// An unreachable Redis says nothing about whether the node is working,
		// so it must not invent a stall.
		return
	}
	if busy == 0 {
		mark()
	}
}
