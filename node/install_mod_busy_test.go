package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A version move, reinstall, restore or copy holds the node_busy key while it
// rewrites the server's tree. A mod written or removed beside it is outside
// that work's stash, so its rollback could not undo it: install_mod refuses
// while the key is held, and remove_mod waits it out (TestRemoveModWaitsOutTheHold).
func TestModCommandsRefuseWhileTheServerIsBusy(t *testing.T) {
	for _, tc := range []struct {
		name string
		busy bool
	}{
		{"busy", true},
		{"idle", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubDownload(t, nil)
			sm, uuid, modsDir := installFixture(t)
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { rdb.Close() })
			if tc.busy {
				mr.Set(nodeBusyKey(uuid), "installing")
			}

			runInstallMod(context.Background(), rdb, sm, replacePayload(uuid, ""))
			_, err := os.Stat(filepath.Join(modsDir, "spark-1.1.jar"))
			if installed := err == nil; installed == tc.busy {
				t.Errorf("busy=%v: install wrote the jar = %v", tc.busy, installed)
			}

			runRemoveMod(context.Background(), rdb, sm, mustModPayload(t, removeModPayload{
				UUID: uuid, ActiveSubServer: "survival", TargetDir: "mods", FileName: "spark-1.0.jar",
			}))
			_, err = os.Stat(filepath.Join(modsDir, "spark-1.0.jar"))
			if kept := err == nil; kept != tc.busy {
				t.Errorf("busy=%v: remove kept the jar = %v", tc.busy, kept)
			}
		})
	}
}
