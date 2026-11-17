package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const unmountedUUID = "abcdef01-2345-4678-9abc-def012345678"

// T08: a disk that did not mount leaves its empty mountpoint behind, and that
// directory passed every check a mounted disk did. A path this node marked
// before and that now lacks the marker while it is empty is skipped.
func TestNewStorageManagerSkipsAnUnmountedKnownPath(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(t *testing.T, disk2 string)
		wantKept bool
	}{
		{"still mounted", func(*testing.T, string) {}, true},
		{"empty mountpoint left behind", func(t *testing.T, disk2 string) {
			if err := os.RemoveAll(disk2); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(disk2, 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"marker lost but data there", func(t *testing.T, disk2 string) {
			if err := os.Remove(filepath.Join(disk2, storageMarkerName)); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(disk2, unmountedUUID), 0o755); err != nil {
				t.Fatal(err)
			}
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			disk1, disk2 := t.TempDir(), t.TempDir()
			csv := disk1 + "," + disk2
			if sm := NewStorageManager(csv, nil); len(sm.Paths()) != 2 || sm.skipped != 0 {
				t.Fatalf("first boot took %v (skipped %d), want both paths", sm.Paths(), sm.skipped)
			}
			tc.prepare(t, disk2)

			sm := NewStorageManager(csv, nil)
			kept := false
			for _, p := range sm.Paths() {
				kept = kept || p == disk2
			}
			if kept != tc.wantKept {
				t.Fatalf("paths = %v, want disk2 kept=%v", sm.Paths(), tc.wantKept)
			}
			if wantSkipped := map[bool]int{true: 0, false: 1}[tc.wantKept]; sm.skipped != wantSkipped {
				t.Errorf("skipped = %d, want %d", sm.skipped, wantSkipped)
			}
		})
	}
}

// A fresh empty disk this node never marked is a new disk, not a lost one.
func TestNewStorageManagerTakesANewEmptyPath(t *testing.T) {
	disk1, disk2 := t.TempDir(), t.TempDir()
	NewStorageManager(disk1, nil)
	sm := NewStorageManager(disk1+","+disk2, nil)
	if len(sm.Paths()) != 2 || sm.skipped != 0 {
		t.Fatalf("paths = %v (skipped %d), want the new disk taken", sm.Paths(), sm.skipped)
	}
}

// T09: with no storage key and copies on two paths, a move to the first copy
// read as "already there" and finished a move that never ran.
func TestResolveMigrationRefusesTwoUnkeyedCopiesEitherWay(t *testing.T) {
	for _, target := range []int{0, 1} {
		sm, roots := newPlacementManager(t, 2)
		for _, r := range roots {
			if err := os.MkdirAll(filepath.Join(r, unmountedUUID), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		_, err := sm.resolveMigration(unmountedUUID, roots[target])
		if err == nil || errors.Is(err, errAlreadyOnPath) || !strings.Contains(err.Error(), "refusing the move") {
			t.Errorf("target %d: err = %v, want the two-copies refusal", target, err)
		}
	}
}

// T05: a server whose storage path is not mounted resolves to "", and the
// version move and the copy returned without a report, so Core's optimistic
// rows stayed.
func TestUnmountedStorageReportsTheRefusal(t *testing.T) {
	tests := []struct {
		name string
		run  func(sm *StorageManager, rdbKey func(string) string) string
	}{
		{"version move", func(sm *StorageManager, key func(string) string) string {
			payload, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
				"uuid": unmountedUUID, "activeSubServer": "main",
			}})
			runUpdateServerVersion(context.Background(), sm.rdb, nil, sm,
				NodeCommand{Installer: InstallerConfig{Type: "paper", Version: "1.21.4"}}, string(payload))
			return key("version_update_failed")
		}},
		{"sub-server copy", func(sm *StorageManager, key func(string) string) string {
			payload, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
				"uuid": unmountedUUID, "sourceSubServer": "main", "targetSubServer": "copy",
			}})
			runCopySubServer(context.Background(), sm.rdb, sm, string(payload))
			return key("copy_sub_failed")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newMiniRedis(t)
			sm := &StorageManager{paths: []string{t.TempDir()}, rdb: rdb}
			ctx := context.Background()
			// Keyed to a path that is not mounted: GetServerDir returns "".
			rdb.Set(ctx, "node:"+nodeID+":server:"+unmountedUUID+":storage", filepath.Join(t.TempDir(), "gone"), 0)
			if dir := sm.GetServerDir(unmountedUUID); dir != "" {
				t.Fatalf("GetServerDir = %q, want empty", dir)
			}
			key := tc.run(sm, func(suffix string) string {
				got, _ := rdb.Get(ctx, "dylaris:server:"+unmountedUUID+":"+suffix).Result()
				return got
			})
			if !strings.Contains(key, "storage is not available") {
				t.Errorf("report = %q, want the storage refusal", key)
			}
		})
	}
}
