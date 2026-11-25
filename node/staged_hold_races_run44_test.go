package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"dylaris-pkg/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// R44-04/R44-07: an abort that ran while migrate_push_r2 was uploading
// released the hold, and the push, finishing on its open file, took it again
// for 7h on a server Core had restarted here.
func TestAnAbortDuringThePushKeepsTheWritesFree(t *testing.T) {
	prevWait, prevQuiet := migrateOutWritersWait, migrateOutQuietWindow
	t.Cleanup(func() {
		migrateOutWritersWait, migrateOutQuietWindow = prevWait, prevQuiet
		migrateAborts.Delete(stagingTestUUID)
	})
	migrateOutWritersWait, migrateOutQuietWindow = 2*time.Second, 50*time.Millisecond
	tests := []struct {
		name  string
		multi bool
	}{
		{"single PUT", false},
		{"multipart", true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, stagingTestUUID), 0o755); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(root, stagingTestUUID, "level.dat"), []byte("w"), 0o644)
			sm := &StorageManager{paths: []string{root}}
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			progress := queue.MigrationProgressID(stagingTestUUID, "push-"+strconv.Itoa(i))
			handleMigrateOut(context.Background(), rdb, sm, "tok", stagingTestUUID, progress, 0)
			if st := readMigrationStatusMaybe(rdb, progress); st.Phase != "staged" {
				t.Fatalf("status %+v: want staged", st)
			}
			aborted := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if !aborted { // Core gave up while the bytes were going up
					aborted = true
					handleMigrateAbort(sm, stagingTestUUID, 0)
				}
				w.Header().Set("ETag", `"e"`)
			}))
			defer srv.Close()

			if tc.multi {
				handleMigratePushR2(context.Background(), rdb, sm, "tok", stagingTestUUID, progress, "", []string{srv.URL + "/p1"}, 1<<20)
			} else {
				handleMigratePushR2(context.Background(), rdb, sm, "tok", stagingTestUUID, progress, srv.URL, nil, 0)
			}

			if !aborted {
				t.Fatal("the upload never reached the server")
			}
			if sm.writesWithdrawn(stagingTestUUID) {
				t.Fatal("writes stay withdrawn on a server whose move was aborted")
			}
		})
	}
}

// R44-05/R44-08: an abort between migrate_out's last look at the abort count
// and its hold found nothing to release, and the hold then lasted 7h. The hold
// is taken under the same lock the abort counts under, so either the abort
// sees it or it sees the abort.
func TestTheStagedHoldRefusesAfterAnAbort(t *testing.T) {
	t.Cleanup(func() { migrateAborts.Delete(stagingTestUUID) })
	tests := []struct {
		name    string
		abort   bool
		archive bool
		want    bool
	}{
		{"the move still running", false, true, true},
		{"aborted after the move started", true, true, false},
		{"the archive already gone", false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sm := &StorageManager{paths: []string{root}}
			archive := stagedArchivePath(root, stagingTestUUID)
			os.MkdirAll(filepath.Dir(archive), 0o755)
			if tc.archive {
				os.WriteFile(archive, []byte("zip"), 0o644)
			}
			aborts := migrateAbortCount(stagingTestUUID)
			if tc.abort {
				sm.abortStagedWithdrawal(stagingTestUUID, func() { os.Remove(archive) })
				os.WriteFile(archive, []byte("zip"), 0o644) // a rename landing late
			}

			got := sm.holdStagedWithdrawal(stagingTestUUID, time.Now().Add(time.Hour), aborts, archive)

			if got != tc.want || sm.writesWithdrawn(stagingTestUUID) != tc.want {
				t.Fatalf("hold = %v, withdrawn = %v, want %v", got, sm.writesWithdrawn(stagingTestUUID), tc.want)
			}
		})
	}
}

// R44-06/R44-09: boot re-derived the hold from an archive the move had left
// behind after it was over: a failed cleanup, or the server moved back here
// since, whose live mark is newer than the archive.
func TestBootSkipsAnArchiveWhoseMoveIsOver(t *testing.T) {
	tests := []struct {
		name    string
		live    time.Duration // live mark this long after the archive; 0 none
		cleanup bool
		want    bool
	}{
		{"a staged move", 0, false, true},
		{"live tree older than the archive", -time.Minute, false, true},
		{"moved back here since", time.Minute, false, false},
		{"its cleanup ran and failed", 0, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			archive := stagedArchivePath(root, stagingTestUUID)
			os.MkdirAll(filepath.Dir(archive), 0o755)
			os.MkdirAll(filepath.Join(root, stagingTestUUID), 0o755)
			os.WriteFile(archive, []byte("zip"), 0o644)
			made := time.Now().Add(-time.Hour)
			os.Chtimes(archive, made, made)
			if tc.live != 0 {
				gen := strconv.FormatInt(made.Add(tc.live).UnixNano(), 10)
				os.WriteFile(filepath.Join(root, stagingTestUUID, storageLiveMarker), []byte(gen), 0o600)
			}
			if tc.cleanup {
				os.WriteFile(migrationCleanupMarker(root, stagingTestUUID), nil, 0o644)
			}
			sm := &StorageManager{paths: []string{root}}

			resumeStagedWithdrawals(sm)

			if got := sm.writesWithdrawn(stagingTestUUID); got != tc.want {
				t.Fatalf("writes withdrawn = %v, want %v", got, tc.want)
			}
		})
	}
}

// A failed migrate_cleanup leaves its marker for the boot, and a later move
// out of this node stages past it.
func TestAFailedCleanupMarksTheMoveOver(t *testing.T) {
	prevWait, prevQuiet := migrateOutWritersWait, migrateOutQuietWindow
	t.Cleanup(func() { migrateOutWritersWait, migrateOutQuietWindow = prevWait, prevQuiet })
	migrateOutWritersWait, migrateOutQuietWindow = 2*time.Second, 50*time.Millisecond
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, stagingTestUUID), 0o755)
	os.WriteFile(filepath.Join(root, stagingTestUUID, "level.dat"), []byte("w"), 0o644)
	sm := &StorageManager{paths: []string{root}}
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	progress := queue.MigrationProgressID(stagingTestUUID, "cleanup")
	handleMigrateOut(context.Background(), rdb, sm, "tok", stagingTestUUID, progress, 0)

	prev := removeTree
	removeTree = func(string) error { return os.ErrPermission }
	handleMigrateCleanup(context.Background(), rdb, sm, nil, "tok", stagingTestUUID)
	removeTree = prev
	if _, err := os.Stat(migrationCleanupMarker(root, stagingTestUUID)); err != nil {
		t.Fatalf("a failed cleanup left no marker: %v", err)
	}

	// The server moves back here and out again.
	os.MkdirAll(filepath.Join(root, stagingTestUUID), 0o755)
	os.WriteFile(filepath.Join(root, stagingTestUUID, "level.dat"), []byte("w"), 0o644)
	handleMigrateOut(context.Background(), rdb, sm, "tok", stagingTestUUID, queue.MigrationProgressID(stagingTestUUID, "again"), 0)
	if _, err := os.Stat(migrationCleanupMarker(root, stagingTestUUID)); !os.IsNotExist(err) {
		t.Fatalf("a new move kept the old cleanup marker: %v", err)
	}
}
