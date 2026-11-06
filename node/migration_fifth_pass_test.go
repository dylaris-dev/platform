package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-pkg/queue"
)

// K47/K49: the restart guard counted from the end of staging, and an R2 move's
// push and pull legs each get up to 6h of Core's wait, so a long move outlived
// it and the source restarted the moved-away server at cutover. The push now
// counts as a use of the archive.
func TestAPushedArchiveStillGuardsTheSource(t *testing.T) {
	for _, multipart := range []bool{false, true} {
		t.Run(map[bool]string{false: "single put", true: "multipart"}[multipart], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			defer srv.Close()
			root := stageArchive(t, "0123456789")
			old := time.Now().Add(-stagedArchiveMaxAge - time.Hour)
			if err := os.Chtimes(stagedArchivePath(root, stagingTestUUID), old, old); err != nil {
				t.Fatal(err)
			}
			storage := &StorageManager{paths: []string{root}}
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			progress := queue.MigrationProgressID(stagingTestUUID, "a")

			if multipart {
				handleMigratePushR2(context.Background(), rdb, storage, "tok", stagingTestUUID, progress, "", []string{srv.URL + "/p1", srv.URL + "/p2"}, 5)
			} else {
				handleMigratePushR2(context.Background(), rdb, storage, "tok", stagingTestUUID, progress, srv.URL, nil, 0)
			}

			if st := readMigrationStatus(t, rdb, progress); st.Phase != "pushed" {
				t.Fatalf("phase = %q (%s), want pushed", st.Phase, st.Error)
			}
			if !migrationInFlight(storage, stagingTestUUID) {
				t.Fatal("a just-pushed archive no longer guards the source against a restart")
			}
		})
	}
}

// K48/K51: the single PUT went through http.DefaultClient, so an endpoint that
// took the connection and never answered held a command slot until the node
// restarted. It is cut off like a part PUT is.
func TestASinglePutNobodyAnswersIsCutOff(t *testing.T) {
	prev := partStallTimeout
	t.Cleanup(func() { partStallTimeout = prev })
	partStallTimeout = 300 * time.Millisecond

	tests := []struct {
		name     string
		size     int
		readBody bool
	}{
		// Far beyond what the socket buffers hold, so the writer blocks.
		{"never reads the body", 64 << 20, false},
		{"reads the body and never answers", 1 << 10, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.readBody {
					buf := make([]byte, 4096)
					for {
						if _, err := r.Body.Read(buf); err != nil {
							break
						}
					}
				}
				<-release
			}))
			defer srv.Close()
			defer close(release)

			file := filepath.Join(t.TempDir(), "archive.zip")
			if err := os.WriteFile(file, make([]byte, tc.size), 0o644); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- putFilePresigned(context.Background(), srv.URL, file, &countingWriter{}) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("put: %v, want the stall to cancel it", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("a single PUT nobody answered was not cut off")
			}
		})
	}
}

// K46: a target without room for the announced archive downloaded up to its
// free space four times over before failing with a vague pull error.
func TestAMoveWithoutRoomFailsBeforeThePull(t *testing.T) {
	prev := restoreDiskBudget
	t.Cleanup(func() { restoreDiskBudget = prev })
	restoreDiskBudget = func(string) int64 { return 10 }

	tests := []struct {
		name     string
		size     int64
		wantPull bool
	}{
		{"larger than the free space", 11, false},
		{"fits", 10, true},
		{"not announced by an older Core", 0, true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			progress := queue.MigrationProgressID(stagingTestUUID, string(rune('a'+i)))
			pulled := false
			receiveMovedServer(context.Background(), rdb, "tok", stagingTestUUID, progress, t.TempDir(), tc.size, "migrate_in", "pull failed",
				func(string, int64, func(int64)) error { pulled = true; return errors.New("stop here") })
			if pulled != tc.wantPull {
				t.Fatalf("pulled = %v, want %v", pulled, tc.wantPull)
			}
			if st := readMigrationStatus(t, rdb, progress); !tc.wantPull && !strings.Contains(st.Error, "not enough free space on the target") {
				t.Fatalf("status = %+v, want the free-space refusal", st)
			}
		})
	}
}

// K50: a discard waiting on a running pull lived only in memory. A node restart
// lost it, the redelivered pull ran again, and its copy stayed for good. The
// discard is now on disk: the redelivered attempt refuses, and the reconciler's
// boot sweep finishes the discard.
func TestADiscardSurvivesARestart(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, discardUUID, "world", "level.dat")
	if err := os.MkdirAll(filepath.Dir(world), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(world, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	const progress = "attempt-2"
	markMovedIn(root, discardUUID, progress)
	storage := &StorageManager{paths: []string{root}}

	// The pull is still running, so the discard waits - and the restart ends it.
	migrateInFlight.Store(discardUUID, struct{}{})
	handleMigrateDiscard(storage, nil, discardUUID, progress, 50*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	migrateInFlight.Delete(discardUUID)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	handleMigratePullR2(context.Background(), rdb, storage, "tok", discardUUID, progress, srv.URL, "sha", 4)
	if hits.Load() != 0 {
		t.Fatal("the redelivered pull of a discarded attempt downloaded again")
	}
	if st := readMigrationStatus(t, rdb, progress); st.Phase != "error" {
		t.Fatalf("phase = %q, want error", st.Phase)
	}

	resumeMigrationDiscards(storage, nil)
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(world); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the discard lost in the restart never removed the copy")
}
