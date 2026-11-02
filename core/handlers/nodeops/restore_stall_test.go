package nodeops

import (
	"context"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
)

func queuedRestore() []models.BackupRestore {
	return []models.BackupRestore{{ID: 1, Status: "queued"}}
}

// The hole this closes: once the command is on the node's durable stream, only
// the node's result ever moves the row again. A node that dies while restoring
// leaves "queued" behind for good, and backup RUNS have a reaper for exactly
// this while restores had nothing.
func TestAQueuedRestoreOnAnOfflineNodeIsReported(t *testing.T) {
	rdb, _ := stallRedis(t)
	restores := queuedRestore()

	AnnotateStalledRestores(context.Background(), rdb, offline, 1, "srv-uuid", restores)

	if !restores[0].Stalled {
		t.Fatal("a queued restore on an offline node with no busy key was not reported")
	}
	if restores[0].StallReason == "" {
		t.Error("the reason is the whole point; it must not be empty")
	}
}

// Every false positive the three conditions exist to remove. Each of these is a
// restore that is perfectly fine, and reporting any of them would train an
// operator to ignore the flag.
func TestARestoreThatIsFineIsNotReported(t *testing.T) {
	for _, tc := range []struct {
		name     string
		online   bool
		busy     bool
		restores []models.BackupRestore
	}{
		{
			// The ordinary dispatch window: pushed, not yet claimed, so there is
			// no busy key. Reporting here would fire on every restore.
			name:     "node connected, not started yet",
			online:   true,
			restores: queuedRestore(),
		},
		{
			name:     "node connected and working",
			online:   true,
			busy:     true,
			restores: queuedRestore(),
		},
		{
			// The node holds the key while restoring. A large world on a slow
			// disk is slow, not stalled - which is why no timeout is invented.
			name:     "node offline but the busy key is still alive",
			online:   false,
			busy:     true,
			restores: queuedRestore(),
		},
		{
			name:     "already finished",
			online:   false,
			restores: []models.BackupRestore{{ID: 1, Status: "success"}},
		},
		{
			name:     "already failed",
			online:   false,
			restores: []models.BackupRestore{{ID: 1, Status: "failed"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb, mr := stallRedis(t)
			if tc.busy {
				mr.Set(services.NodeBusyKey("srv-uuid"), "restarting")
			}
			conn := offline
			if tc.online {
				conn = online
			}
			AnnotateStalledRestores(context.Background(), rdb, conn, 1, "srv-uuid", tc.restores)
			for _, r := range tc.restores {
				if r.Stalled {
					t.Errorf("restore %d reported as stalled; it is not", r.ID)
				}
			}
		})
	}
}

// An unreachable Redis says nothing about whether the node is working, so it
// must not be read as a stall. The install-stall sibling makes the same promise.
func TestAnUnreachableRedisDoesNotInventARestoreStall(t *testing.T) {
	rdb, mr := stallRedis(t)
	mr.Close()
	restores := queuedRestore()

	AnnotateStalledRestores(context.Background(), rdb, offline, 1, "srv-uuid", restores)

	if restores[0].Stalled {
		t.Error("a Redis failure was reported as a stalled restore")
	}
}
