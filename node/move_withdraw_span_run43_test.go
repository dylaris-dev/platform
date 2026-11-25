package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// R43-04/R43-05: migrate_out gave the write withdrawal back as soon as it had
// staged, while Beam and SFTP sessions still acted on access from before the
// move, so a write after 'staged' landed in the source tree and was deleted
// at cleanup. The withdrawal now lasts until the move ends on this node.
func TestStagedWithdrawalLastsUntilTheMoveEnds(t *testing.T) {
	prevWait, prevQuiet := migrateOutWritersWait, migrateOutQuietWindow
	t.Cleanup(func() { migrateOutWritersWait, migrateOutQuietWindow = prevWait, prevQuiet })
	migrateOutWritersWait, migrateOutQuietWindow = 2*time.Second, 50*time.Millisecond
	zipBody, sum := movableZip(t)
	tests := []struct {
		name string
		end  func(t *testing.T, rdb *redis.Client, sm *StorageManager)
		want bool
	}{
		{"staged, the move still running", func(*testing.T, *redis.Client, *StorageManager) {}, true},
		{"aborted: the server stays here", func(_ *testing.T, _ *redis.Client, sm *StorageManager) {
			handleMigrateAbort(sm, stagingTestUUID, 0)
		}, false},
		{"cleaned up: the server is gone from here", func(_ *testing.T, rdb *redis.Client, sm *StorageManager) {
			handleMigrateCleanup(context.Background(), rdb, sm, nil, "tok", stagingTestUUID)
		}, false},
		{"moved back here before the cleanup", func(t *testing.T, rdb *redis.Client, sm *StorageManager) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(zipBody) }))
			defer srv.Close()
			back := queue.MigrationProgressID(stagingTestUUID, "back")
			handleMigratePullR2(context.Background(), rdb, sm, "tok", stagingTestUUID, back, srv.URL, sum, int64(len(zipBody)))
			if st := readMigrationStatus(t, rdb, back); st.Phase != "transferred" {
				t.Fatalf("move back phase = %q (%s)", st.Phase, st.Error)
			}
		}, false},
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
			progress := queue.MigrationProgressID(stagingTestUUID, string(rune('a'+i)))

			handleMigrateOut(context.Background(), rdb, sm, "tok", stagingTestUUID, progress, 0)
			if st := readMigrationStatusMaybe(rdb, progress); st.Phase != "staged" {
				t.Fatalf("status %+v: want staged", st)
			}
			tc.end(t, rdb, sm)

			if got := sm.writesWithdrawn(stagingTestUUID); got != tc.want {
				t.Fatalf("writes withdrawn = %v, want %v", got, tc.want)
			}
		})
	}
}

// A restart between 'staged' and the end of the move must not give the
// writes back: the withdrawal is re-derived from the staged archive on disk,
// bounded by the archive's own age limit.
func TestStagedWithdrawalIsReDerivedAtBoot(t *testing.T) {
	tests := []struct {
		name string
		file string
		age  time.Duration
		want bool
	}{
		{"a staged archive", stagingTestUUID + ".zip", time.Minute, true},
		{"an archive older than any move", stagingTestUUID + ".zip", stagedArchiveMaxAge + time.Minute, false},
		{"a half-written archive", stagingTestUUID + ".x.part", time.Minute, false},
		{"no archive", "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, migrationStagingDir)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.file != "" {
				p := filepath.Join(dir, tc.file)
				if err := os.WriteFile(p, []byte("zip"), 0o644); err != nil {
					t.Fatal(err)
				}
				at := time.Now().Add(-tc.age)
				os.Chtimes(p, at, at)
			}
			sm := &StorageManager{paths: []string{root}}

			resumeStagedWithdrawals(sm)

			if got := sm.writesWithdrawn(stagingTestUUID); got != tc.want {
				t.Fatalf("writes withdrawn = %v, want %v", got, tc.want)
			}
		})
	}
}
