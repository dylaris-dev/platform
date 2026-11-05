package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func forgetTombstoneInMemory(uuid string) {
	deletedServers.Lock()
	delete(deletedServers.at, uuid)
	deletedServers.Unlock()
}

// A setup or reinstall the queue redelivers after a node restart must still
// see the delete that ran before the restart.
func TestDeleteTombstoneSurvivesARestart(t *testing.T) {
	cases := []struct {
		name      string
		secretDir bool
		age       time.Duration
		want      bool
	}{
		{"fresh tombstone, persisted", true, 0, true},
		{"aged out", true, deletedServerMemory + time.Hour, false},
		{"no secret dir: memory only", false, 0, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uuid := "7d1e0000-0000-4000-8000-00000000000" + string(rune('1'+i))
			prev := nodeSecretDir
			nodeSecretDir = ""
			if c.secretDir {
				nodeSecretDir = t.TempDir()
			}
			t.Cleanup(func() { nodeSecretDir = prev; forgetTombstoneInMemory(uuid) })

			markServerDeleted(uuid)
			if c.age > 0 {
				old := time.Now().Add(-c.age)
				os.Chtimes(tombstonePath(uuid), old, old)
			}
			forgetTombstoneInMemory(uuid) // the restart
			if got := serverDeleted(uuid); got != c.want {
				t.Fatalf("serverDeleted after restart = %v, want %v", got, c.want)
			}
		})
	}
}

// Old markers are pruned so the directory does not grow for good.
func TestDeleteTombstonePrunesOldMarkers(t *testing.T) {
	prev := nodeSecretDir
	nodeSecretDir = t.TempDir()
	const oldUUID, newUUID = "7d1e0000-0000-4000-8000-0000000000a1", "7d1e0000-0000-4000-8000-0000000000a2"
	t.Cleanup(func() { nodeSecretDir = prev; forgetTombstoneInMemory(oldUUID); forgetTombstoneInMemory(newUUID) })
	markServerDeleted(oldUUID)
	old := time.Now().Add(-deletedServerMemory - time.Hour)
	os.Chtimes(tombstonePath(oldUUID), old, old)
	markServerDeleted(newUUID)
	if _, err := os.Stat(tombstonePath(oldUUID)); !os.IsNotExist(err) {
		t.Fatalf("aged-out marker still on disk: %v", err)
	}
}

// A create queued with (or redelivered after) a delete of the same server
// must not build its directory or container.
func TestCreateAfterDeleteBuildsNothing(t *testing.T) {
	const uuid = sharedDaemonUUID
	t.Cleanup(func() { forgetTombstoneInMemory(uuid) })
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	dm, d := newSharedDaemon(t, "-", false, nil)
	root := t.TempDir()
	storage := NewStorageManager(root, rdb)
	dm.storageMgr = storage

	markServerDeleted(uuid)
	forgetTombstoneInMemory(uuid) // and the node restarted since

	cmd := NodeCommand{Action: "create"}
	cmd.Config.UUID = uuid
	cmd.Config.Docker.Image = "img:1"
	processCommand(context.Background(), cmd, "", rdb, dm, "node-a", nil, storage)

	if ch := d.changed(); strings.Contains(strings.Join(ch, ","), "create") && ch[len(ch)-1] != "remove new-id-0123456789ab" {
		t.Fatalf("a container was created for a deleted server: %v", ch)
	}
	if _, err := os.Stat(filepath.Join(root, uuid)); !os.IsNotExist(err) {
		t.Fatalf("a directory was created for a deleted server: %v", err)
	}
}

// A delete during the image pull must not leave the directory the container
// build creates after the pull.
func TestADeleteDuringThePullLeavesNoDirectory(t *testing.T) {
	const uuid = sharedDaemonUUID
	t.Cleanup(func() { forgetTombstoneInMemory(uuid) })
	dm, _ := newSharedDaemon(t, "-", false, func() { markServerDeleted(uuid) })
	cfg := ServerConfig{UUID: uuid}
	cfg.Docker.Image = "img:1"
	if err := dm.RecreateWithCommand(cfg); err == nil {
		t.Fatal("no error for a server deleted during its pull")
	}
	if _, err := os.Stat(filepath.Join(dm.localDataPath, "servers", uuid)); !os.IsNotExist(err) {
		t.Fatalf("directory recreated for a server deleted during its pull: %v", err)
	}
}

// A delete whose container removal failed, or a build that slipped past the
// tombstone, left an mc_<uuid> nothing would ever remove. The reconciler now
// removes this node's containers of servers deleted here, and nothing else.
func TestALeftoverContainerOfADeletedServerIsRemoved(t *testing.T) {
	const uuid = sharedDaemonUUID
	for _, c := range []struct {
		owner   string
		deleted bool
		want    bool
	}{
		{"node-a", true, true},
		{"node-a", false, false},
		{"node-b", true, false}, // another node's on a shared daemon
	} {
		dm, d := newSharedDaemon(t, c.owner, true, nil)
		forgetTombstoneInMemory(uuid)
		if c.deleted {
			markServerDeleted(uuid)
		}
		left := dm.removeDeletedLeftovers([]MCContainerInfo{{UUID: uuid, ContainerName: "mc_" + uuid}})
		removed := strings.Contains(strings.Join(d.changed(), ","), "remove mc_"+uuid)
		if removed != c.want || (len(left) == 0) != c.deleted {
			t.Errorf("owner %q deleted %v: removed %v (%v), left %v", c.owner, c.deleted, removed, d.changed(), left)
		}
		forgetTombstoneInMemory(uuid)
	}
	b, err := os.ReadFile("docker_mgr.go")
	if err != nil {
		t.Fatal(err)
	}
	fallback := between(t, string(b), "starting without them\", containerName, err)", "dm.openNetGate(config.UUID)")
	if !strings.Contains(fallback, "serverDeleted(config.UUID)") {
		t.Error("the extra-port fallback starts a second container without checking for a delete")
	}
}
