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
		// The queue redelivers a pending setup whatever its age.
		{"node down over a long weekend", true, 4 * 24 * time.Hour, true},
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
		// By the inspected ID, not the name (ownedRef).
		removed := strings.Contains(strings.Join(d.changed(), ","), "remove c-id")
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

// P05: Core lets a deleted server's UUID come back (the create takes a UUID
// from the client), and the tombstone then refused that server for 90 days. A
// create Core issued after the delete clears it; anything that cannot show
// that - an old Core without issuedAt, an old tombstone without the delete's,
// a create from before the delete - is still refused.
func TestACreateIssuedAfterTheDeleteClearsTheTombstone(t *testing.T) {
	cases := []struct {
		name          string
		deleteIssued  int64
		createIssued  int64
		restart       bool
		wantClearance bool
	}{
		{"create issued after the delete", 1000, 2000, false, true},
		{"after a node restart", 1000, 2000, true, true},
		{"create queued before the delete", 1000, 990, false, false},
		{"same second", 1000, 1000, false, false},
		{"old core: create has no issuedAt", 1000, 0, false, false},
		{"old tombstone: no delete issuedAt", 0, 2000, true, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uuid := "7d1e0000-0000-4000-8000-0000000005a" + string(rune('0'+i))
			prev := nodeSecretDir
			nodeSecretDir = t.TempDir()
			t.Cleanup(func() { nodeSecretDir = prev; forgetTombstoneInMemory(uuid) })

			markServerDeletedIssued(uuid, c.deleteIssued)
			if c.restart {
				forgetTombstoneInMemory(uuid)
			}
			if got := clearTombstoneForCreate(uuid, c.createIssued); got != c.wantClearance {
				t.Fatalf("cleared = %v, want %v", got, c.wantClearance)
			}
			if serverDeleted(uuid) == c.wantClearance {
				t.Fatalf("serverDeleted = %v after clearance %v", serverDeleted(uuid), c.wantClearance)
			}
		})
	}
}

// T01: a move Core issued after the delete brings the server back just like a
// create does. Without clearing the tombstone the move landed, Core cut over
// and removed the source copy, and nothing here could build the container.
// A move that cannot show it came after the delete fails before any byte is
// pulled, so Core rolls back while the source still has the server.
func TestAMoveIssuedAfterTheDeleteClearsTheTombstone(t *testing.T) {
	cases := []struct {
		name         string
		tombstone    bool
		deleteIssued int64
		moveIssued   int64
		wantAdmit    bool
	}{
		{"no tombstone", false, 0, 0, true},
		{"move issued after the delete", true, 1000, 2000, true},
		{"move queued before the delete", true, 1000, 990, false},
		{"old core: move has no issuedAt", true, 1000, 0, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uuid := "7d1e0000-0000-4000-8000-0000000006a" + string(rune('0'+i))
			prev := nodeSecretDir
			nodeSecretDir = t.TempDir()
			t.Cleanup(func() { nodeSecretDir = prev; forgetTombstoneInMemory(uuid) })
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			if c.tombstone {
				markServerDeletedIssued(uuid, c.deleteIssued)
			}

			if got := admitMoveOfDeletedServer(context.Background(), rdb, "tok", uuid, uuid, c.moveIssued); got != c.wantAdmit {
				t.Fatalf("admitted = %v, want %v", got, c.wantAdmit)
			}
			if serverDeleted(uuid) == c.wantAdmit {
				t.Fatalf("serverDeleted = %v after admit %v", serverDeleted(uuid), c.wantAdmit)
			}
			st := readMigrationStatusMaybe(rdb, uuid)
			if c.wantAdmit && st.Phase != "" {
				t.Fatalf("an admitted move wrote status %q", st.Phase)
			}
			if !c.wantAdmit && st.Phase != "error" {
				t.Fatalf("a refused move wrote phase %q, want error so Core rolls back", st.Phase)
			}
		})
	}
}

// W01: a tombstone file that cannot be removed used to lift the tombstone in
// memory only, so the very next serverDeleted fell through to the file and
// the create built a server only to discard it, or a move landed that could
// never start. The clearance and serverDeleted must agree.
func TestATombstoneThatCannotBeRemovedIsNotCleared(t *testing.T) {
	uuid := "7d1e0000-0000-4000-8000-0000000007a0"
	prev := nodeSecretDir
	nodeSecretDir = t.TempDir()
	t.Cleanup(func() { nodeSecretDir = prev; forgetTombstoneInMemory(uuid) })

	markServerDeletedIssued(uuid, 1000)
	// A non-empty directory in the file's place makes os.Remove fail.
	if err := os.Remove(tombstonePath(uuid)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tombstonePath(uuid), "x"), 0o700); err != nil {
		t.Fatal(err)
	}

	if clearTombstoneForCreate(uuid, 2000) {
		t.Fatal("cleared although the tombstone file is still there")
	}
	if !serverDeleted(uuid) {
		t.Fatal("serverDeleted = false, the tombstone is still on disk")
	}
}
