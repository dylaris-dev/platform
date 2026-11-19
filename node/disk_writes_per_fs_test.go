package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The tests share one write count whatever directory they use: the temp dirs
// and the working directory can sit on different filesystems in a container,
// and the tests written against one node-wide count mean "this disk".
func TestMain(m *testing.M) {
	fsIDOf = func(string) uint64 { return 0 }
	os.Exit(m.Run())
}

// A spool or restore held on one disk was charged against every disk: a
// restore onto an untouched second storage path was refused as out of space
// while a large server was spooled on the first, and the spool counted the
// second disk's writes against its own free space.
func TestWritesCountOnlyAgainstTheirOwnFilesystem(t *testing.T) {
	const gb = 1_000_000_000
	diskA, diskB := t.TempDir(), t.TempDir()
	prevID, prevBudget, prevSpool := fsIDOf, restoreDiskBudget, globalSpoolDir
	t.Cleanup(func() {
		fsIDOf, restoreDiskBudget, globalSpoolDir = prevID, prevBudget, prevSpool
		diskWritesByFS.Range(func(k, _ any) bool { diskWritesByFS.Delete(k); return true })
	})
	fsIDOf = func(dir string) uint64 {
		if rel, err := filepath.Rel(diskB, dir); err == nil && filepath.IsLocal(rel) || dir == diskB {
			return 2
		}
		return 1
	}
	restoreDiskBudget = func(string) int64 { return 100 * gb }
	globalSpoolDir = filepath.Join(diskA, ".dylaris-backup-spool")
	on := &backupPolicy{SpoolEnabled: true, SpoolMaxGB: 500}

	held, release, removed, why := reserveSpool(on, 150*gb, freeIs(2000*gb))
	if held <= 0 {
		t.Fatalf("spool refused: %s", why)
	}
	b := nodeWriteBudget(filepath.Join(diskB, "srv"))
	if b.left != 100*gb {
		t.Errorf("budget on disk B = %d with a spool held on disk A, want the whole %d", b.left, int64(100*gb))
	}
	if a := nodeWriteBudget(filepath.Join(diskA, "srv")); a.left >= 100*gb {
		t.Errorf("budget on disk A = %d, want the spool's hold taken out", a.left)
	}
	if !b.spend(90 * gb) {
		t.Fatal("90 GB on disk B refused")
	}
	release(150 * gb)
	if !b.spend(5 * gb) {
		t.Error("the spool landing on disk A was charged to a write running on disk B")
	}
	b.release()
	removed()

	// The reverse: a write held on disk B does not shrink the spool disk.
	c := nodeWriteBudget(filepath.Join(diskB, "srv"))
	c.spend(1900 * gb)
	defer c.release()
	if held, release, _, why := reserveSpool(on, 150*gb, freeIs(200*gb)); held <= 0 {
		t.Errorf("spool on disk A refused for a write on disk B: %s", why)
	} else {
		release(0)
	}
}

func TestFSIDOfAMissingDirIsItsParents(t *testing.T) {
	dir := t.TempDir()
	if got, want := fsID(filepath.Join(dir, "not", "yet")), fsID(dir); got != want {
		t.Errorf("fsID of a missing dir = %d, want its existing parent's %d", got, want)
	}
}
