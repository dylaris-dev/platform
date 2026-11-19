package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// R38-07: a copy on storage another node also uses, with no container on this
// daemon, may be that node's live server - migrate_cleanup leaves the moved
// server's copy there on purpose. Recreating it here ran two servers on one
// world.
func TestTheDeletedPassLeavesSharedStorageAlone(t *testing.T) {
	for _, shared := range []bool{false, true} {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		mr.Set(desiredStateKey(sharedDaemonUUID), "online")
		root := t.TempDir()
		serverDir := filepath.Join(root, sharedDaemonUUID)
		if err := os.MkdirAll(filepath.Join(serverDir, "main"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(serverDir, ".node_config.json"),
			[]byte(`{"uuid":"`+sharedDaemonUUID+`","activeSubServer":"main","docker":{"image":"img:1","ram":1024}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if shared {
			if err := writeBeacon(root, nodeBeacon{NodeID: "other-node", UpdatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
		dm, d := newSharedDaemon(t, "-", false, nil)
		storage := &StorageManager{paths: []string{root}}
		dm.storageMgr = storage

		reconcileDeletedContainers(t.Context(), rdb, dm, storage)

		if started := slices.Contains(d.changed(), "start new-id-0123456789ab"); started == shared {
			t.Errorf("shared=%v: started=%v (%v)", shared, started, d.changed())
		}
	}
}
