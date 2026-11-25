package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// R43-03: migrate_storage stopped and restarted the server without its power
// lock, so a restart already in the command stream could bring the JVM up on
// the source tree while the move copied it, and the move then deleted it. The
// move now waits for a power action to finish, holds the lock through the
// copy, and a cancelled wait leaves the lock and the tree alone.
func TestStorageMoveHoldsThePowerLockOfItsServer(t *testing.T) {
	const uuid = sharedDaemonUUID
	tests := []struct {
		name   string
		cancel bool // the node shuts down while the move waits for the lock
	}{
		{"waits for a running power action, then moves under the lock", false},
		{"a cancelled wait moves nothing and holds nothing", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rdb, _ := newBusyRedis(t)
			dm, _ := newSharedDaemon(t, "node-a", false, nil)
			storage, roots := newPlacementManager(t, 2)
			storage.rdb = rdb
			dm.storageMgr = storage
			if err := os.MkdirAll(filepath.Join(roots[0], uuid), 0o755); err != nil {
				t.Fatal(err)
			}
			storage.setRedisPath(uuid, roots[0])

			copied, lockFreeMidCopy := false, false
			prev := migrateCopyTree
			t.Cleanup(func() { migrateCopyTree = prev })
			migrateCopyTree = func(src, dst string) error {
				copied = true
				if release, ok := tryLockServerPower(uuid); ok {
					lockFreeMidCopy = true
					release()
				}
				return prev(src, dst)
			}

			// A restart of the server is running when the move comes in.
			holdRestart, ok := tryLockServerPower(uuid)
			if !ok {
				t.Fatal("power lock not free at the start")
			}
			var once sync.Once
			releaseRestart := func() { once.Do(holdRestart) }
			t.Cleanup(releaseRestart)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				cmd := NodeCommand{Action: "migrate_storage", TargetPath: roots[1]}
				cmd.Config.UUID = uuid
				processCommand(ctx, cmd, "", rdb, dm, "node-a", nil, storage)
			}()
			select {
			case <-done:
				t.Fatal("the move ran while a power action of its server held the lock")
			case <-time.After(500 * time.Millisecond):
			}
			if copied {
				t.Fatal("the move copied the tree while a power action held the lock")
			}
			if tc.cancel {
				cancel()
				<-done
				releaseRestart()
			} else {
				releaseRestart()
				<-done
			}

			if lockFreeMidCopy {
				t.Error("a power action could take the server's lock in the middle of the move")
			}
			if copied == tc.cancel {
				t.Errorf("copied = %v, want %v", copied, !tc.cancel)
			}
			if release, ok := tryLockServerPower(uuid); !ok {
				t.Error("the move did not release the power lock")
			} else {
				release()
			}
			if got, _ := rdb.Get(context.Background(), nodeBusyKey(uuid)).Result(); got == "installing" {
				t.Error("the move left its node_busy hold behind")
			}
		})
	}
}
