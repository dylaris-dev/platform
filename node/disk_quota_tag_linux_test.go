package main

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// The tag walk runs as root over a tree the tenant writes to while its server
// runs. An entry that was a directory when its parent was listed and is a
// symlink by the time it is opened must not be followed: chattr -R and
// xfs_quota did, and tagged whatever the link named (another tenant's tree,
// the storage root) with the tenant's project.
func TestWalkProjectTreeNeverLeavesTheTree(t *testing.T) {
	for _, tc := range []struct {
		name string
		// swap runs when the walk visits dir "a", after "a" was listed.
		swap bool
	}{
		{"symlink present from the start", false},
		{"directory swapped for a symlink mid-walk", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			tree := filepath.Join(base, "tree")
			outside := filepath.Join(base, "victim")
			for _, d := range []string{filepath.Join(tree, "a", "b"), outside} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range []string{filepath.Join(tree, "a", "b", "world.dat"), filepath.Join(tree, "server.jar"), filepath.Join(outside, "save.dat")} {
				if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, filepath.Join(tree, "link")); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(tree, "pipe"), 0o644); err != nil {
				t.Fatal(err)
			}
			if !tc.swap {
				if err := os.RemoveAll(filepath.Join(tree, "a", "b")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(tree, "a", "b")); err != nil {
					t.Fatal(err)
				}
			}
			aIno := inodeOf(t, filepath.Join(tree, "a"))
			outIno := map[uint64]string{inodeOf(t, outside): "victim", inodeOf(t, filepath.Join(outside, "save.dat")): "victim/save.dat"}
			seen := map[uint64]bool{}
			err := walkProjectTree(tree, func(fd int, dir bool) error {
				var st syscall.Stat_t
				if err := syscall.Fstat(fd, &st); err != nil {
					return err
				}
				seen[st.Ino] = true
				if tc.swap && dir && st.Ino == aIno {
					b := filepath.Join(tree, "a", "b")
					if err := os.RemoveAll(b); err != nil {
						return err
					}
					return os.Symlink(outside, b)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			for ino, name := range outIno {
				if seen[ino] {
					t.Errorf("the walk followed a symlink and tagged %s outside the tree", name)
				}
			}
			for _, p := range []string{tree, filepath.Join(tree, "a"), filepath.Join(tree, "server.jar")} {
				if !seen[inodeOf(t, p)] {
					t.Errorf("%s was not tagged", p)
				}
			}
		})
	}
}

func inodeOf(t *testing.T, p string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ino
}

// R25-08: a tenant can nest directories far below PATH_MAX's reach (the walk
// opens by parent fd), and every level held its own full path string and two
// fds, so memory grew with the square of the depth. Directories deeper than
// maxProjectTreeDepth are counted as failed and not entered.
func TestWalkProjectTreeStopsAtTheDepthCap(t *testing.T) {
	prev := maxProjectTreeDepth
	maxProjectTreeDepth = 3
	t.Cleanup(func() { maxProjectTreeDepth = prev })
	for _, tc := range []struct {
		depth      int
		wantErr    bool
		wantVisits int // directories and files visited, root included
	}{
		{3, false, 1 + 3*2},
		{5, true, 1 + 3*2},
	} {
		t.Run(strconv.Itoa(tc.depth), func(t *testing.T) {
			root := t.TempDir()
			d := root
			for i := 0; i < tc.depth; i++ {
				d = filepath.Join(d, "d")
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			visits := 0
			err := walkProjectTree(root, func(int, bool) error { visits++; return nil })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if visits != tc.wantVisits {
				t.Fatalf("visited %d entries, want %d", visits, tc.wantVisits)
			}
		})
	}
}
