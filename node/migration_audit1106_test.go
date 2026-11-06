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

	"dylaris-pkg/migration"
	"dylaris-pkg/queue"
)

// M17: migrate_out archived the source while a Beam or SFTP write was still
// landing - Core revokes those rights only on their next access check - and
// the source was deleted after cutover, so the write was lost or torn. A held
// writer now refuses the staging, and a write that began while the tree was
// read drops the archive.
func TestAMigrateOutDoesNotStageOverAWrite(t *testing.T) {
	tests := []struct {
		name string
		// before holds a writer across the whole run; during takes and
		// releases one while the archive is written.
		before, during bool
		wantErr        string
	}{
		{"a write in progress", true, false, "in progress"},
		{"a write while archiving", false, true, "written to the server while the move was staging"},
		{"no write", false, false, ""},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, stagingTestUUID), 0o755); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(root, stagingTestUUID, "level.dat"), []byte("w"), 0o644)
			storage := &StorageManager{paths: []string{root}}
			if tc.before {
				defer storage.holdWriter(stagingTestUUID)()
			}
			prev := migrateOutArchive
			t.Cleanup(func() { migrateOutArchive = prev })
			archived := false
			migrateOutArchive = func(src, dst string) (string, int64, error) {
				archived = true
				if tc.during {
					storage.holdWriter(stagingTestUUID)()
				}
				return migration.Archive(src, dst)
			}
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			progress := queue.MigrationProgressID(stagingTestUUID, string(rune('a'+i)))

			handleMigrateOut(context.Background(), rdb, storage, "tok", stagingTestUUID, progress)

			st := readMigrationStatusMaybe(rdb, progress)
			_, zipErr := os.Stat(stagedArchivePath(root, stagingTestUUID))
			if tc.wantErr == "" {
				if st.Phase != "staged" || zipErr != nil {
					t.Fatalf("status %+v, archive %v: want staged", st, zipErr)
				}
				return
			}
			if st.Phase != "error" || !strings.Contains(st.Error, tc.wantErr) {
				t.Fatalf("status %+v, want an error naming %q", st, tc.wantErr)
			}
			if zipErr == nil {
				t.Fatal("an archive that may miss a write was put in place")
			}
			if tc.before && archived {
				t.Fatal("archived while a write was in progress")
			}
		})
	}
}

// M27: the discard removed the copy and its storage key, so the redelivered
// pull's lookup fell back to the first storage path, found no marker there
// (it sat beside the copy on the second) and transferred the attempt again.
func TestADiscardedAttemptIsRefusedOnAnyStoragePath(t *testing.T) {
	tests := []struct {
		name    string
		marker  string // attempt named by the marker on the second path
		attempt string
		want    bool
	}{
		{"this attempt was discarded", "A", "A", true},
		{"another attempt was", "A", "B", false},
		{"none was", "", "A", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			m := migrationDiscardMarker(b, stagingTestUUID)
			if tc.marker != "" {
				if err := os.WriteFile(m, []byte(queue.MigrationProgressID(stagingTestUUID, tc.marker)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			storage := &StorageManager{paths: []string{a, b}}
			progress := queue.MigrationProgressID(stagingTestUUID, tc.attempt)
			if got := refuseDiscardedAttempt(context.Background(), rdb, storage, "tok", stagingTestUUID, progress); got != tc.want {
				t.Fatalf("refused = %v, want %v", got, tc.want)
			}
			if _, err := os.Stat(m); tc.marker != "" && !tc.want && !os.IsNotExist(err) {
				t.Errorf("another attempt's marker was kept: %v", err)
			}
		})
	}
}

// M28: a migrate_out redelivered after a restart mid-staging carries Core's
// original stamp, so a staging over five minutes was failed as "queued too
// long ago" while Core was still waiting on it.
func TestARestartedStagingIsNotDroppedAsStale(t *testing.T) {
	tests := []struct {
		name   string
		status string // written by this node; "" writes none
		age    time.Duration
		want   bool // dropped
	}{
		{"never started", "", 0, true},
		{"staging a moment ago", `{"phase":"staging","bytes":7}`, time.Minute, false},
		{"staging long ago", `{"phase":"staging","bytes":7}`, migrateOutMaxAge + time.Minute, true},
		{"another phase", `{"phase":"error","error":"x"}`, time.Minute, true},
	}
	now := time.Unix(1_800_000_000, 0)
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			progress := queue.MigrationProgressID(stagingTestUUID, string(rune('a'+i)))
			if tc.status != "" {
				rdb.Set(context.Background(), queue.MigrationStatusKey("tok", progress), tc.status, migrationStatusTTL)
				mr.FastForward(tc.age)
			}
			if got := dropStaleMigrateOut(context.Background(), rdb, "tok", progress, now.Add(-time.Hour).Unix(), now); got != tc.want {
				t.Fatalf("dropped = %v, want %v", got, tc.want)
			}
		})
	}
}
