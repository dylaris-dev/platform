package main

import (
	"context"
	"errors"
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

// L43: the target checked the announced archive alone against its free space,
// but the archive stays on disk while it is unpacked next to it. A target with
// room for one copy and not two downloaded the whole archive, for hours on the
// R2 path, and then failed the unpack.
func TestAMoveNeedsRoomForTheArchiveAndItsUnpackedCopy(t *testing.T) {
	prev := restoreDiskBudget
	t.Cleanup(func() { restoreDiskBudget = prev })
	restoreDiskBudget = func(string) int64 { return 10 }

	tests := []struct {
		name     string
		size     int64
		wantPull bool
	}{
		{"room for the archive only", 6, false},
		{"room for both", 5, true},
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

// L44: the boot sweeps kept every temp file younger than six or seven hours,
// so the half-written archive or download of a move the restart had just
// killed held its space against the redelivered attempt, which then refused
// for want of room. Nothing this process writes predates its start.
func TestTheBootSweepsDropTheTempFilesOfTheProcessBefore(t *testing.T) {
	prev := nodeStartedAt
	t.Cleanup(func() { nodeStartedAt = prev })
	nodeStartedAt = time.Now()
	dead := nodeStartedAt.Add(-time.Minute)

	root := t.TempDir()
	staging := filepath.Join(root, migrationStagingDir)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		path     string
		old      bool
		wantGone bool
	}{
		{"a dead staging part", filepath.Join(staging, stagingTestUUID+".123.part"), true, true},
		{"this process's staging part", filepath.Join(staging, stagingTestUUID+".456.part"), false, false},
		{"a fresh staged archive", stagedArchivePath(root, stagingTestUUID), true, false},
		{"a dead pull", filepath.Join(root, ".migration-pull-789"), true, true},
		{"a dead received archive", filepath.Join(root, stagingTestUUID+".migration-in.zip"), true, true},
		{"this process's pull", filepath.Join(root, ".migration-pull-000"), false, false},
	}
	for _, tc := range tests {
		if err := os.WriteFile(tc.path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if tc.old {
			if err := os.Chtimes(tc.path, dead, dead); err != nil {
				t.Fatal(err)
			}
		}
	}

	sweepStaleMigrationArchives(&StorageManager{paths: []string{root}})
	sweepInterruptedWork([]string{root})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := os.Stat(tc.path)
			if gone := os.IsNotExist(err); gone != tc.wantGone {
				t.Fatalf("gone = %v, want %v", gone, tc.wantGone)
			}
		})
	}
}

// L46: migrate_out measured the whole server before it reported anything, and
// Core's stage wait restarts only on reported progress: a tree too large to
// measure inside it failed every move. The measurement now reports as it goes.
func TestMigrateOutReportsWhileItMeasures(t *testing.T) {
	prevInterval, prevMeasure, prevArchive := stagingProgressInterval, migrateOutMeasure, migrateOutArchive
	t.Cleanup(func() {
		stagingProgressInterval, migrateOutMeasure, migrateOutArchive = prevInterval, prevMeasure, prevArchive
	})
	stagingProgressInterval = 2 * time.Millisecond

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stagingTestUUID), 0o755); err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	progress := queue.MigrationProgressID(stagingTestUUID, "a")

	var sawProgress bool
	migrateOutMeasure = func(_ string, counted *atomic.Int64) int64 {
		counted.Add(5)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if st := readMigrationStatusMaybe(rdb, progress); st.Phase == "staging" && st.Bytes == 5 {
				sawProgress = true
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		return counted.Load()
	}
	migrateOutArchive = func(string, string) (string, int64, error) { return "", 0, errors.New("stop here") }

	handleMigrateOut(context.Background(), rdb, &StorageManager{paths: []string{root}}, "tok", stagingTestUUID, progress)
	if !sawProgress {
		t.Fatal("nothing reported while the server was measured")
	}
}
