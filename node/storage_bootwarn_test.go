package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// assertBoots checks the boot rule (R26 ruling): the identity-disk check never
// refuses a boot. A first path that reads as unmounted gets a warning naming
// it and the remount hint, and the StorageManager of the boot running from it
// keeps it. The markers are put back afterwards, so the caller's layout is
// unchanged.
func assertBoots(t *testing.T, csv, first string) {
	t.Helper()
	msg := identityDirWarning(csv)
	if unmounted := identityDirUnmounted(csv); unmounted != (msg != "") {
		t.Fatalf("identityDirUnmounted = %v but warning = %q", unmounted, msg)
	}
	if msg != "" && (!strings.Contains(msg, first) || !strings.Contains(msg, "mount the disk")) {
		t.Fatalf("warning = %q, want the path %s and the remount hint", msg, first)
	}
	paths := strings.Split(csv, ",")
	markers := map[string][]byte{}
	for _, p := range paths {
		if b, err := os.ReadFile(filepath.Join(p, storageMarkerName)); err == nil {
			markers[p] = b
		}
	}
	prev := nodeSecretDir
	nodeSecretDir = first
	t.Cleanup(func() { nodeSecretDir = prev })
	sm := NewStorageManager(csv, nil)
	nodeSecretDir = prev
	if !slices.Contains(sm.Paths(), first) {
		t.Fatalf("paths = %v, want the first path %s kept", sm.Paths(), first)
	}
	for _, p := range paths {
		f := filepath.Join(p, storageMarkerName)
		if b, ok := markers[p]; ok {
			if err := os.WriteFile(f, b, 0o644); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// R26-08: a volume another node used is attached as the second path. Its
// current marker records that node's id and a higher gen, so this node's own
// first path read as stale and its real .node_id lost to the foreign id: the
// boot was refused and the node crash-looped. It boots, warns once, and the
// marker it writes clears the warning for the next boot.
func TestAForeignMarkerDoesNotRefuseTheBoot(t *testing.T) {
	disk1, disk2 := t.TempDir(), t.TempDir()
	csv := disk1 + "," + disk2
	for f, body := range map[string]string{
		filepath.Join(disk1, ".node_id"):        "id1\n",
		filepath.Join(disk1, storageMarkerName): disk1 + "\n" + disk2 + "\n#kept 5 " + disk1 + "\n#kept 5 " + disk2 + "\n#node id1\n#gen 5\n",
		filepath.Join(disk2, storageMarkerName): disk1 + "\n#kept 9 " + disk1 + "\n#node id2\n#gen 9\n",
		filepath.Join(disk1, "sftp_host_key"):   "k",
	} {
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(disk2, "srv"), 0o755); err != nil {
		t.Fatal(err)
	}
	assertBoots(t, csv, disk1)
	prev := nodeSecretDir
	nodeSecretDir = disk1
	t.Cleanup(func() { nodeSecretDir = prev })
	if sm := NewStorageManager(csv, nil); len(sm.Paths()) != 2 {
		t.Fatalf("paths = %v, want both kept", sm.Paths())
	}
	if msg := identityDirWarning(csv); msg != "" {
		t.Fatalf("the next boot still warns: %q", msg)
	}
}

// R26-09: a path whose directory cannot be read at runtime listed no servers,
// and with no storageUnavailable entry Core showed every server on it as
// missing from disk and offered to remove it from the DB.
func TestUnavailablePathsIncludesAnUnreadablePath(t *testing.T) {
	ok := t.TempDir()
	if err := os.Mkdir(filepath.Join(ok, "srv"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file where a directory belongs: statfs answers, ReadDir fails, as on
	// a stale NFS handle or an I/O error.
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(broken, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sm := &StorageManager{paths: []string{ok, broken}}
	sm.skipPath("/skipped/at/boot")
	infos := sm.GetStorageInfo()
	got := sm.UnavailablePaths(infos)
	want := []string{"/skipped/at/boot", broken}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("UnavailablePaths = %v, want %v", got, want)
	}
	if infos[0].ServerCount != 1 {
		t.Fatalf("readable path lists %d servers, want 1", infos[0].ServerCount)
	}
	if healthy := (&StorageManager{paths: []string{ok}}); len(healthy.UnavailablePaths(healthy.GetStorageInfo())) != 0 {
		t.Fatal("a healthy node reports unavailable paths, which suppresses Core's missing list")
	}
}

// R27-05/R27-06: the R26-08 layout with a third path. This node's own second
// path is healthy and empty, and the foreign disk at the third path lists it
// with a higher #kept gen. The foreign marker made it read as stale, so it was
// skipped, and the foreign gen written back kept it skipped after the foreign
// disk was gone.
func TestAForeignMarkerDoesNotSkipAnOwnEmptyPath(t *testing.T) {
	disk1, disk2, disk3 := t.TempDir(), t.TempDir(), t.TempDir()
	csv := disk1 + "," + disk2 + "," + disk3
	own := disk1 + "\n" + disk2 + "\n" + disk3 + "\n#kept 5 " + disk1 + "\n#kept 5 " + disk2 + "\n#kept 5 " + disk3 + "\n#node id1\n#gen 5\n"
	for f, body := range map[string]string{
		filepath.Join(disk1, ".node_id"):        "id1\n",
		filepath.Join(disk1, storageMarkerName): own,
		filepath.Join(disk2, storageMarkerName): own,
		filepath.Join(disk3, storageMarkerName): disk1 + "\n" + disk2 + "\n#kept 9 " + disk1 + "\n#kept 9 " + disk2 + "\n#node id2\n#gen 9\n",
	} {
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(disk3, "srv"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := nodeSecretDir
	nodeSecretDir = disk1
	t.Cleanup(func() { nodeSecretDir = prev })
	sm := NewStorageManager(csv, nil)
	if !slices.Contains(sm.Paths(), disk2) || len(sm.Paths()) != 3 {
		t.Fatalf("paths = %v, want all three kept", sm.Paths())
	}
	// The foreign disk is detached: its mountpoint is bare again. The own
	// empty path must still be taken, and only the bare mountpoint skipped.
	if err := os.RemoveAll(disk3); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(disk3, 0o755); err != nil {
		t.Fatal(err)
	}
	sm = NewStorageManager(csv, nil)
	if got, want := sm.Paths(), []string{disk1, disk2}; !slices.Equal(got, want) {
		t.Fatalf("after the foreign disk left, paths = %v, want %v", got, want)
	}
}
