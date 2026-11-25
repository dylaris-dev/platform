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
		// V13: the first storage path is nodeSecretDir, and a boot that dropped
		// it still wrote the node's own root files onto the mountpoint.
		{"empty mountpoint holding the node's root files", func(t *testing.T, disk2 string) {
			if err := os.RemoveAll(disk2); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(disk2, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{"sftp_host_key", unmountedUUID + ".counted", unmountedUUID + ".deleted"} {
				if err := os.WriteFile(filepath.Join(disk2, f), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
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

// V10: a boot that took an empty mountpoint (the first boot of this version,
// or every path unmounted at once) wrote the marker onto the root disk's
// directory. Hidden under the mount while the disk is there, it vouched for
// the path the next time the disk was missing.
func TestAMarkerWrittenOntoTheMountpointDoesNotVouchForIt(t *testing.T) {
	disk1, mnt := t.TempDir(), t.TempDir()
	csv := disk1 + "," + mnt
	// Boot A: the disk is not mounted, mnt is the bare mountpoint.
	if sm := NewStorageManager(csv, nil); len(sm.Paths()) != 2 {
		t.Fatalf("boot A took %v, want both", sm.Paths())
	}
	// Boot B: the disk mounts over it, holding a server.
	under := mnt + "-under"
	if err := os.Rename(mnt, under); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mnt, unmountedUUID), 0o755); err != nil {
		t.Fatal(err)
	}
	if sm := NewStorageManager(csv, nil); len(sm.Paths()) != 2 {
		t.Fatalf("boot B took %v, want both", sm.Paths())
	}
	// Boot C: the disk is missing again; the mountpoint shows boot A's marker.
	if err := os.Rename(mnt, mnt+"-disk"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(under, mnt); err != nil {
		t.Fatal(err)
	}
	sm := NewStorageManager(csv, nil)
	for _, p := range sm.Paths() {
		if p == mnt {
			t.Fatalf("paths = %v, the unmounted path was taken on its stale marker", sm.Paths())
		}
	}
	// Boot D: mounted again, its own (older) marker and its server are back.
	if err := os.Rename(mnt, under); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(mnt+"-disk", mnt); err != nil {
		t.Fatal(err)
	}
	if sm := NewStorageManager(csv, nil); len(sm.Paths()) != 2 {
		t.Fatalf("boot D took %v, want the remounted disk back", sm.Paths())
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
			runCopySubServer(context.Background(), sm.rdb, sm, nil, string(payload))
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

// B10: the node's identity (.node_id, .node_secret, .node_key) lives on the
// first storage path, and it is read before the StorageManager runs. A first
// disk that did not mount has to be noticed there, or the node enrols afresh
// on the bare mountpoint.
func TestIdentityDirUnmounted(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, disk1, disk2 string)
		want    bool
	}{
		{"both mounted", func(*testing.T, string, string) {}, false},
		{"first disk not mounted", func(t *testing.T, disk1, _ string) {
			if err := os.RemoveAll(disk1); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(disk1, 0o755); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"second disk not mounted", func(t *testing.T, _, disk2 string) {
			if err := os.RemoveAll(disk2); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(disk2, 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"operator emptied the marker on a new first disk", func(t *testing.T, disk1, _ string) {
			if err := os.RemoveAll(disk1); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(disk1, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(disk1, storageMarkerName), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			disk1, disk2 := t.TempDir(), t.TempDir()
			csv := disk1 + "," + disk2
			NewStorageManager(csv, nil)
			tc.prepare(t, disk1, disk2)
			if got := identityDirUnmounted(csv); got != tc.want {
				t.Fatalf("identityDirUnmounted = %v, want %v", got, tc.want)
			}
			assertBoots(t, csv, disk1)
		})
	}
	single := t.TempDir()
	if identityDirUnmounted(single) {
		t.Fatal("a single path has nothing to vouch against and must read as mounted")
	}
	assertBoots(t, single, single)
}

// R22-06/07: markers written by earlier releases (gen-less 2026.11.17.2,
// numbered without #kept lines from 2026.11.18) leave a healthy first path
// reading as stale. The refusal guards a node that would enrol afresh, so a
// first path that already holds the identity files must always boot.
func TestIdentityDirUnmountedBootsAnExistingIdentity(t *testing.T) {
	layouts := []struct {
		name string
		// markers returns the marker bodies for disk1 and disk2; nil means no file.
		markers   func(a, b string) (ma, mb *string)
		unmounted bool // what the marker rule alone says for a first path without identity
	}{
		{"2026.11.18 gens without kept lines, stale first", func(a, b string) (*string, *string) {
			return ptr(a + "\n" + b + "\n#gen 1\n"), ptr(a + "\n" + b + "\n#gen 2\n")
		}, true},
		{"2026.11.17.2 gen-less first beside a numbered one", func(a, b string) (*string, *string) {
			return ptr(a + "\n" + b + "\n"), ptr(a + "\n" + b + "\n#gen 1\n")
		}, true},
		{"stale gen with a kept line for another gen", func(a, b string) (*string, *string) {
			return ptr(a + "\n" + b + "\n#kept 1 " + a + "\n#gen 1\n"), ptr(a + "\n" + b + "\n#kept 3 " + a + "\n#kept 3 " + b + "\n#gen 3\n")
		}, true},
		{"no marker of its own, listed by the second", func(a, b string) (*string, *string) {
			return nil, ptr(a + "\n" + b + "\n#gen 4\n")
		}, true},
		{"gen-less markers on both", func(a, b string) (*string, *string) {
			return ptr(a + "\n" + b + "\n"), ptr(a + "\n" + b + "\n")
		}, false},
		{"current gens with kept lines", func(a, b string) (*string, *string) {
			body := a + "\n" + b + "\n#kept 2 " + a + "\n#kept 2 " + b + "\n#gen 2\n"
			return ptr(body), ptr(body)
		}, false},
		{"operator override: empty marker on the first", func(a, b string) (*string, *string) {
			return ptr(""), ptr(a + "\n" + b + "\n#gen 5\n")
		}, false},
	}
	// R24-10: only .node_id and .node_secret stop a fresh enrolment. .node_key
	// is written before the node enrols and .redis_addr is a cache, so a first
	// path holding only those is a bare mountpoint as far as the boot rule goes.
	identities := []struct {
		files []string
		boots bool
	}{
		{nil, false},
		{[]string{".node_id"}, true},
		{[]string{".node_secret"}, true},
		{[]string{".node_id", ".node_secret", ".node_key", ".redis_addr"}, true},
		{[]string{".node_key"}, false},
		{[]string{".redis_addr"}, false},
		{[]string{".node_key", ".redis_addr"}, false},
	}
	for _, l := range layouts {
		for _, id := range identities {
			name := l.name + "/identity " + strings.Join(id.files, "+")
			if len(id.files) == 0 {
				name = l.name + "/no identity"
			}
			t.Run(name, func(t *testing.T) {
				disk1, disk2 := t.TempDir(), t.TempDir()
				ma, mb := l.markers(disk1, disk2)
				for _, m := range []struct {
					dir  string
					body *string
				}{{disk1, ma}, {disk2, mb}} {
					if m.body != nil {
						if err := os.WriteFile(filepath.Join(m.dir, storageMarkerName), []byte(*m.body), 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}
				// What a live first path carries beside the identity.
				if err := os.WriteFile(filepath.Join(disk1, "sftp_host_key"), []byte("k"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(disk2, "srv"), 0o755); err != nil {
					t.Fatal(err)
				}
				want := l.unmounted && !id.boots
				for _, f := range id.files {
					if err := os.WriteFile(filepath.Join(disk1, f), []byte("x"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if got := identityDirUnmounted(disk1 + "," + disk2); got != want {
					t.Fatalf("identityDirUnmounted = %v, want %v", got, want)
				}
				assertBoots(t, disk1+","+disk2, disk1)
				// The StorageManager must answer as the boot did, and heal the
				// marker so the next boot no longer reads the path as stale.
				for boot := 1; boot <= 2; boot++ {
					sm := NewStorageManager(disk1+","+disk2, nil)
					if kept := len(sm.Paths()) == 2; kept == want {
						t.Fatalf("boot %d: kept %v, want first path kept=%v (paths %v)", boot, sm.Paths(), !want, sm.Paths())
					}
				}
			})
		}
	}
	single := t.TempDir()
	if err := os.WriteFile(filepath.Join(single, storageMarkerName), []byte(single+"\n#gen 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if identityDirUnmounted(single) {
		t.Fatal("a single path has nothing to vouch against and must read as mounted")
	}
	assertBoots(t, single, single)
}

func ptr(s string) *string { return &s }

// R24-11: an identity left on the bare mountpoint by a boot that ran there
// (a fresh .node_key, or a phantom enrolment's .node_id and .node_secret)
// vouched for the path, so the node booted on it and dropUnmountedPaths gave it
// a current marker. Once a marker records the node id, only the matching
// .node_id counts, and a real disk whose own marker went stale still boots.
func TestIdentityDirUnmountedChecksTheRecordedNodeID(t *testing.T) {
	tests := []struct {
		name  string
		bare  bool              // disk1 is the bare mountpoint, not the real disk
		stale bool              // disk1 shows the marker of the boot before the last
		files map[string]string // identity files on disk1 at the next boot
		want  bool
	}{
		{"bare mountpoint with a fresh key", true, true, map[string]string{".node_key": "k"}, true},
		{"bare mountpoint with a phantom enrolment", true, true, map[string]string{".node_id": "phantom", ".node_secret": "s", ".node_key": "k"}, true},
		{"bare mountpoint holding the real id", true, true, map[string]string{".node_id": "real"}, false},
		{"real disk with its own marker stale", false, true, map[string]string{".node_id": "real", ".node_secret": "s"}, false},
		{"real disk re-paired as a new node", false, false, map[string]string{".node_id": "new"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			disk1, disk2 := t.TempDir(), t.TempDir()
			csv := disk1 + "," + disk2
			if err := os.WriteFile(filepath.Join(disk1, ".node_id"), []byte("real\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			NewStorageManager(csv, nil)
			old, err := os.ReadFile(filepath.Join(disk1, storageMarkerName))
			if err != nil {
				t.Fatal(err)
			}
			NewStorageManager(csv, nil)
			if b, _ := os.ReadFile(filepath.Join(disk2, storageMarkerName)); !strings.Contains(string(b), storageMarkerNode+"real\n") {
				t.Fatalf("marker = %q, want the node id recorded", b)
			}
			if tc.bare {
				if err := os.Rename(disk1, disk1+"-disk"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(disk1, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(filepath.Join(disk1, ".node_id")); err != nil {
				t.Fatal(err)
			}
			if tc.stale {
				if err := os.WriteFile(filepath.Join(disk1, storageMarkerName), old, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for f, v := range tc.files {
				if err := os.WriteFile(filepath.Join(disk1, f), []byte(v), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := identityDirUnmounted(csv); got != tc.want {
				t.Fatalf("identityDirUnmounted = %v, want %v", got, tc.want)
			}
			assertBoots(t, csv, disk1)
			sm := NewStorageManager(csv, nil)
			if kept := len(sm.Paths()) == 2; kept == tc.want {
				t.Fatalf("paths = %v, want first path kept=%v", sm.Paths(), !tc.want)
			}
			if b, _ := os.ReadFile(filepath.Join(disk1, storageMarkerName)); tc.want && string(b) != string(old) {
				t.Fatalf("the skipped path's marker was rewritten to %q", b)
			}
			if id, ok := tc.files[".node_id"]; ok && !tc.want {
				if b, _ := os.ReadFile(filepath.Join(disk2, storageMarkerName)); !strings.Contains(string(b), storageMarkerNode+id+"\n") {
					t.Fatalf("marker = %q, want %s recorded", b, id)
				}
			}
		})
	}
}

// R25-09: the Redis bootstrap runs between identityDirUnmounted and
// dropUnmountedPaths and can replace .node_id (a re-enrolment mints a new id).
// The first path identityDirUnmounted vouched for under the old id was then
// dropped as unmounted, the markers kept the old id, and every later boot
// refused the node. The identity dir this boot runs from is kept, and the
// marker records the id the node now runs as.
func TestAnIdentityReplacedAtBootKeepsTheFirstPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		newID string
	}{
		{"same id", "A"},
		{"re-enrolled as a new id", "B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disk1, disk2 := t.TempDir(), t.TempDir()
			csv := disk1 + "," + disk2
			prev := nodeSecretDir
			nodeSecretDir = disk1
			t.Cleanup(func() { nodeSecretDir = prev })
			if err := os.WriteFile(filepath.Join(disk1, ".node_id"), []byte("A\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			NewStorageManager(csv, nil)
			old, err := os.ReadFile(filepath.Join(disk1, storageMarkerName))
			if err != nil {
				t.Fatal(err)
			}
			NewStorageManager(csv, nil)
			// disk1's own marker went stale (a failed write), it holds no server.
			if err := os.WriteFile(filepath.Join(disk1, storageMarkerName), old, 0o644); err != nil {
				t.Fatal(err)
			}
			if identityDirUnmounted(csv) {
				t.Fatal("identityDirUnmounted refused a first path holding the recorded id")
			}
			assertBoots(t, csv, disk1)
			if err := os.WriteFile(filepath.Join(disk1, ".node_id"), []byte(tc.newID+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if sm := NewStorageManager(csv, nil); len(sm.Paths()) != 2 {
				t.Fatalf("paths = %v, want the identity dir kept", sm.Paths())
			}
			b, _ := os.ReadFile(filepath.Join(disk2, storageMarkerName))
			if !strings.Contains(string(b), storageMarkerNode+tc.newID+"\n") || strings.Count(string(b), storageMarkerNode) != 1 {
				t.Fatalf("marker = %q, want only %s recorded", b, tc.newID)
			}
			if identityDirUnmounted(csv) {
				t.Fatal("the next boot refuses the node")
			}
		})
	}
}
