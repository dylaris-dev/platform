package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// V11: migrate_storage took node_busy before CheckMigration and dropped it on
// the refusal. The key is shared, so a refused move deleted the interlock a
// running restore (or install) held, and the reconciler started the server
// in the middle of it.
func TestARefusedStorageMoveKeepsAnotherHoldersBusyKey(t *testing.T) {
	const uuid = sharedDaemonUUID
	ctx := context.Background()
	rdb, _ := newBusyRedis(t)
	dm, _ := newSharedDaemon(t, "node-a", false, nil)
	storage, roots := newPlacementManager(t, 2)
	storage.rdb = rdb
	dm.storageMgr = storage
	if err := os.MkdirAll(filepath.Join(roots[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	storage.setRedisPath(uuid, roots[0])

	tests := []struct {
		name   string
		refuse func(t *testing.T) func()
	}{
		{"a writer holds the tree", func(*testing.T) func() { return storage.holdWriter(uuid) }},
		{"a folder download reads it", func(*testing.T) func() { return storage.holdReader(uuid) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			release := tc.refuse(t)
			defer release()
			rdb.Set(ctx, nodeBusyKey(uuid), "restarting", busyStatusTTL)

			cmd := NodeCommand{Action: "migrate_storage", TargetPath: roots[1]}
			cmd.Config.UUID = uuid
			processCommand(ctx, cmd, "", rdb, dm, "node-a", nil, storage)

			got, _ := rdb.Get(ctx, nodeBusyKey(uuid)).Result()
			if got != "restarting" {
				t.Fatalf("node_busy = %q after the refused move, want the restore's hold kept", got)
			}
		})
	}
}
