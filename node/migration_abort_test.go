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

	"dylaris-pkg/queue"
)

// A failed move drops the archive it staged - a full copy of the server
// outside its quota that only a successful move's cleanup used to remove - and
// nothing else: the server stays on this node with its data.
func TestMigrateAbortDropsTheArchiveAndKeepsTheServer(t *testing.T) {
	root := t.TempDir()
	const uuid = "srv-abort"
	world := filepath.Join(root, uuid, "world", "level.dat")
	if err := os.MkdirAll(filepath.Dir(world), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(world, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, migrationStagingDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{stagedArchivePath(root, uuid), migrationOriginPath(root, uuid)} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	handleMigrateAbort(&StorageManager{paths: []string{root}}, uuid, 0)

	for _, p := range []string{stagedArchivePath(root, uuid), migrationOriginPath(root, uuid)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s is still there", p)
		}
	}
	if _, err := os.Stat(world); err != nil {
		t.Fatalf("the server's own data went with it: %v", err)
	}
}

// A server whose move this node staged is not restarted by the reconciler:
// the desired state turns "online" for the target, and the source restarted
// its stopped container on data the cleanup was about to delete.
func TestTheReconcilerLeavesAServerThatIsMovingAway(t *testing.T) {
	root := t.TempDir()
	const uuid = "srv-moving"
	if err := os.MkdirAll(filepath.Join(root, uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	storage := &StorageManager{paths: []string{root}}
	if restartBlocked(context.Background(), rdb, storage, uuid) {
		t.Fatal("an ordinary stopped server is blocked")
	}
	if err := os.MkdirAll(filepath.Join(root, migrationStagingDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedArchivePath(root, uuid), []byte("zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !restartBlocked(context.Background(), rdb, storage, uuid) {
		t.Fatal("a server with a staged move would be restarted")
	}
}

// One transfer into a server at a time: an attempt Core gave up on keeps
// running, and two extracting into one directory wrecked each other.
func TestASecondTransferIntoTheSameServerIsRefused(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	migrateInFlight.Store("srv-busy", struct{}{})
	defer migrateInFlight.Delete("srv-busy")
	progress := queue.MigrationProgressID("srv-busy", "a2")
	handleMigrateIn(context.Background(), rdb, &StorageManager{paths: []string{t.TempDir()}}, "tok", "srv-busy", progress, "src", "token", "sha", 10, nil)
	got, _ := rdb.Get(context.Background(), queue.MigrationStatusKey("tok", progress)).Result()
	if !strings.Contains(got, "still running") {
		t.Fatalf("status %q, want the second transfer refused", got)
	}
}

// The arriving copy lands on an empty directory: a copy left by an earlier
// move merged with it, and files deleted on the source since came back.
func TestAnArrivingCopyLandsOnAnEmptyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "srv")
	stale := filepath.Join(dir, "plugins", "removed.jar")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := resetMigrationTarget(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a stale file survived")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatal("the directory itself is gone")
	}
}

// An archive older than any move is not a move. Every attempt before moves
// worked left one, and while it counted the server was never restarted after
// a crash again. The sweep removes them.
func TestAStaleArchiveDoesNotHoldTheServerAndIsSwept(t *testing.T) {
	root := t.TempDir()
	const uuid = "srv-stale"
	if err := os.MkdirAll(filepath.Join(root, uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, migrationStagingDir), 0o755); err != nil {
		t.Fatal(err)
	}
	zip := stagedArchivePath(root, uuid)
	if err := os.WriteFile(zip, []byte("zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-stagedArchiveMaxAge - time.Minute)
	if err := os.Chtimes(zip, old, old); err != nil {
		t.Fatal(err)
	}
	storage := &StorageManager{paths: []string{root}}
	if migrationInFlight(storage, uuid) {
		t.Fatal("a stale archive still holds the server")
	}
	sweepStaleMigrationArchives(storage)
	if _, err := os.Stat(zip); !os.IsNotExist(err) {
		t.Fatal("the stale archive was not swept")
	}
}

// A cleanup that waited in a busy queue must not delete a server that is being
// moved back onto this node at the same time.
func TestACleanupLeavesAServerArrivingAgain(t *testing.T) {
	root := t.TempDir()
	const uuid = "srv-back"
	world := filepath.Join(root, uuid, "level.dat")
	if err := os.MkdirAll(filepath.Dir(world), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(world, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	migrateInFlight.Store(uuid, struct{}{})
	defer migrateInFlight.Delete(uuid)
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	handleMigrateCleanup(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, "tok", uuid)
	if _, err := os.Stat(world); err != nil {
		t.Fatal("the cleanup deleted a server arriving again")
	}
}

// The target of a move that did not happen drops the copy it pulled - only a
// copy a move brought (the marker), only once no pull is still writing it, and
// never one a container runs from.
func TestMigrateDiscardDropsOnlyAMovedInCopy(t *testing.T) {
	seed := func(t *testing.T, marked bool) (string, string) {
		root := t.TempDir()
		world := filepath.Join(root, discardUUID, "world", "level.dat")
		if err := os.MkdirAll(filepath.Dir(world), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(world, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		if marked {
			markMovedIn(root, discardUUID, "attempt-2")
		}
		return root, world
	}
	gone := func(p string) bool {
		for i := 0; i < 100; i++ {
			if _, err := os.Stat(p); os.IsNotExist(err) {
				return true
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
	}

	t.Run("a copy a move brought is removed", func(t *testing.T) {
		root, world := seed(t, true)
		handleMigrateDiscard(&StorageManager{paths: []string{root}}, nil, discardUUID, "attempt-2", time.Second)
		if !gone(world) || !gone(movedInMarker(root, discardUUID)) {
			t.Fatal("the copy of a move that did not happen was kept")
		}
	})

	t.Run("a directory no move brought is kept", func(t *testing.T) {
		root, world := seed(t, false)
		handleMigrateDiscard(&StorageManager{paths: []string{root}}, nil, discardUUID, "attempt-2", time.Second)
		time.Sleep(200 * time.Millisecond)
		if _, err := os.Stat(world); err != nil {
			t.Fatal("a server directory without the move marker was removed")
		}
	})

	// A discard for an earlier attempt that arrives late must not take the
	// copy a later, successful attempt brought.
	t.Run("another attempt's discard keeps the copy", func(t *testing.T) {
		root, world := seed(t, true)
		handleMigrateDiscard(&StorageManager{paths: []string{root}}, nil, discardUUID, "attempt-1", time.Second)
		time.Sleep(200 * time.Millisecond)
		if _, err := os.Stat(world); err != nil {
			t.Fatal("a discard for another attempt removed this attempt's copy")
		}
	})

	t.Run("a pull still running is waited for", func(t *testing.T) {
		root, world := seed(t, true)
		migrateInFlight.Store(discardUUID, struct{}{})
		handleMigrateDiscard(&StorageManager{paths: []string{root}}, nil, discardUUID, "attempt-2", time.Minute)
		time.Sleep(200 * time.Millisecond)
		if _, err := os.Stat(world); err != nil {
			migrateInFlight.Delete(discardUUID)
			t.Fatal("removed while a pull was still writing it")
		}
		migrateInFlight.Delete(discardUUID)
	})
}

const discardUUID = "7d0a1c2e-3b4f-4a5b-8c6d-9e0f1a2b3c4d"

// R37-10: a successful move away removes every copy of the server on this
// node, not only the one the storage key names: a second copy left by an
// unsettled storage move was brought back up here by the reconciler. A copy on
// storage another node also uses may be the target's live one and stays.
func TestMigrateCleanupRemovesEveryCopyOnTheSource(t *testing.T) {
	prevNodeID := nodeID
	t.Cleanup(func() { nodeID = prevNodeID })
	nodeID = "node-src"
	const uuid = "9a1b2c3d-4e5f-4a6b-8c7d-0e1f2a3b4c5d"
	cases := []struct {
		name        string
		sharedOther bool // the second path is also another node's storage
		wantOther   bool // the second copy survives
	}{
		{"leftover on own storage goes", false, false},
		{"copy on storage shared with the target stays", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			p1, p2 := t.TempDir(), t.TempDir()
			for _, p := range []string{p1, p2} {
				if err := os.MkdirAll(filepath.Join(p, uuid), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p, uuid, ".node_config.json"), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.sharedOther {
				if err := writeBeacon(p2, nodeBeacon{NodeID: "node-dst", UpdatedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			sm := &StorageManager{paths: []string{p1, p2}, rdb: rdb}
			if err := sm.setRedisPath(uuid, p1); err != nil {
				t.Fatal(err)
			}
			handleMigrateCleanup(context.Background(), rdb, sm, nil, "tok", uuid)
			if _, err := os.Stat(filepath.Join(p1, uuid)); !os.IsNotExist(err) {
				t.Fatal("the keyed copy was not removed")
			}
			_, err := os.Stat(filepath.Join(p2, uuid))
			if got := err == nil; got != tc.wantOther {
				t.Fatalf("second copy survives = %v, want %v", got, tc.wantOther)
			}
		})
	}
}
