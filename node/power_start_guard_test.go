package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A kill pressed while a start or restart was recreating the container (the
// image pull, the create) hit a container that was gone or not yet started,
// failed, and the recreate then started the server against the user's click.
func TestAKillDuringTheRecreateLeavesTheServerStopped(t *testing.T) {
	const uuid = sharedDaemonUUID
	for _, c := range []struct {
		action string
		pullN  int32 // the pull the kill lands during
	}{
		{"start", 2}, // the first is PullContainerImage, before the check
		{"restart", 1},
	} {
		t.Run(c.action, func(t *testing.T) {
			lastStopIssued.Delete(uuid)
			t.Cleanup(func() { lastStopIssued.Delete(uuid) })
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			var pulls atomic.Int32
			dm, d := newSharedDaemon(t, "node-a", false, func() {
				if pulls.Add(1) == c.pullN {
					noteStop(uuid, 2001) // the kill, on its express worker
				}
			})
			storage := NewStorageManager(t.TempDir(), rdb)
			dm.storageMgr = storage

			cmd := NodeCommand{Action: c.action, IssuedAt: 2000}
			cmd.Config.UUID = uuid
			processCommand(context.Background(), cmd, "", rdb, dm, "node-a", nil, storage)

			ch := strings.Join(d.changed(), ",")
			if !strings.Contains(ch, "create") {
				t.Fatalf("the kill did not land inside the recreate: %v", ch)
			}
			if strings.Contains(ch, "start new-id") {
				t.Fatalf("%s started the container after a newer kill: %v", c.action, ch)
			}
			if got, _ := rdb.Get(context.Background(), "dylaris:server:"+uuid+":status").Result(); got != "stopped" {
				t.Errorf("status %q, want stopped", got)
			}
		})
	}
}

// The reconciler's recreate of a missing container ran without the power lock
// and never looked at the desired state again, so a stop or kill sent during
// its pull was undone.
func TestTheReconcilersRecreateYieldsToAStop(t *testing.T) {
	const uuid = sharedDaemonUUID
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	key := desiredStateKey(uuid)

	for _, c := range []struct {
		name       string
		stopInPull bool
		wantStart  bool
	}{
		{"still wanted online", false, true},
		{"stopped during the pull", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			mr.Set(key, "online")
			dm, d := newSharedDaemon(t, "-", false, func() {
				if c.stopInPull {
					mr.Set(key, "stopped")
				}
			})
			cfg := ServerConfig{UUID: uuid}
			cfg.Docker.Image = "img:1"
			ran, err := reconcileStart(ctx, rdb, uuid, func() error { return dm.RecreateWithCommand(cfg) })
			if !ran {
				t.Fatal("did not run with the lock free")
			}
			started := strings.Contains(strings.Join(d.changed(), ","), "start new-id")
			if started != c.wantStart || (err != nil) == c.wantStart {
				t.Fatalf("started = %v, err = %v; want started %v (%v)", started, err, c.wantStart, d.changed())
			}
		})
	}

	release, ok := lockServerPower(uuid, "start", nil)
	if !ok {
		t.Fatal("lock")
	}
	defer release()
	if ran, _ := reconcileStart(ctx, rdb, uuid, func() error { t.Fatal("ran beside a command"); return nil }); ran {
		t.Fatal("reconcileStart ran while a command held the lock")
	}
}

// A settings change that recreated a running server during a kill keeps its
// new container, stopped, and reports success so the config is saved.
func TestASettingsRecreateDuringAKillKeepsTheChangeStopped(t *testing.T) {
	const uuid = sharedDaemonUUID
	dm, d := newSharedDaemon(t, "node-a", true, nil)
	defer startGuard(uuid, func() bool { return true })()
	cfg := ServerConfig{UUID: uuid}
	cfg.Docker.Image = "img:1"
	if err := dm.RecreateKeepingRunState(cfg); err != nil {
		t.Fatalf("RecreateKeepingRunState: %v", err)
	}
	ch := strings.Join(d.changed(), ",")
	if !strings.Contains(ch, "create") || strings.Contains(ch, "start new-id") {
		t.Fatalf("want a new container left stopped, got %v", ch)
	}
}
