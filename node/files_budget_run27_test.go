package main

import (
	"os"
	"path/filepath"
	"testing"
)

// R27-04: the sub-server copy (copy_sub_server, through copyDir) removed its
// staging on failure, but the budget had already booked the copied bytes as
// landed and noted, and remeasured with the staging still on disk.
func TestAFailedSubServerCopyBooksNothingAsLanded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget int64
	}{
		{"fails mid-file", 16 << 10},
		{"fails on the first file", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, uuid, root := seedNodeOwned(t)
			if err := os.WriteFile(filepath.Join(root, "survival", "world.dat"), make([]byte, 64<<10), 0o644); err != nil {
				t.Fatal(err)
			}
			withDiskBudget(t, tc.budget, 1000)
			prev := serverDiskHeadroom
			serverDiskHeadroom = func(string) (string, int64, bool) { return uuid, 1 << 40, true }
			t.Cleanup(func() { serverDiskHeadroom = prev })

			disk := writesOn("")
			l0, n0 := disk.landed.Load(), notedFor(uuid).Load()
			staging := filepath.Join(root, ".creative.copying")
			if err := copyDir(filepath.Join(root, "survival"), staging); err == nil {
				t.Fatal("a copy larger than the free space succeeded")
			}
			if _, err := os.Lstat(staging); !os.IsNotExist(err) {
				t.Errorf("the failed copy's staging is still on disk: %v", err)
			}
			if got := disk.landed.Load() - l0; got != 0 {
				t.Errorf("%d removed bytes booked as landed", got)
			}
			if got := notedFor(uuid).Load() - n0; got != 0 {
				t.Errorf("%d removed bytes noted on the server", got)
			}
			if g, w := disk.inflight.Load(), disk.onDisk.Load(); g != 0 || w != 0 {
				t.Errorf("still held: inflight %d, onDisk %d", g, w)
			}
		})
	}
}
