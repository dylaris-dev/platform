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
			err := walkProjectTree(tree, "", func(fd int, dir bool) error {
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
			err := walkProjectTree(root, "", func(int, bool) error { visits++; return nil })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if visits != tc.wantVisits {
				t.Fatalf("visited %d entries, want %d", visits, tc.wantVisits)
			}
		})
	}
}

// B06: the server's tag walk leaves its node-local backup folder alone, so the
// archives are not charged to the server's project; TagBackupDir decides that
// folder on its own. Only the top-level folder: a tenant's own directory of
// that name deeper in the tree is theirs and counts.
func TestTagWalkSkipsTheBackupFolder(t *testing.T) {
	root := t.TempDir()
	backups := filepath.Join(root, backupDirName)
	nested := filepath.Join(root, "world", backupDirName)
	for _, d := range []string{backups, nested} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "run-1.tar.gz"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[uint64]bool{}
	if err := walkProjectTree(root, backupDirName, func(fd int, _ bool) error {
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			return err
		}
		seen[st.Ino] = true
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{backups, false},
		{filepath.Join(backups, "run-1.tar.gz"), false},
		{nested, true},
		{filepath.Join(nested, "run-1.tar.gz"), true},
	} {
		if got := seen[inodeOf(t, tc.path)]; got != tc.want {
			t.Errorf("%s visited = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// pid 0 untags: without clearing inheritance, the next archive written into
// the folder took the project back.
func TestProjectAttrs(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   fsxattr
		pid  uint32
		dir  bool
		want fsxattr
	}{
		{"tag a directory", fsxattr{}, 7, true, fsxattr{projid: 7, xflags: fsXflagProjinherit}},
		{"tag a file", fsxattr{}, 7, false, fsxattr{projid: 7}},
		{"untag a directory", fsxattr{projid: 7, xflags: fsXflagProjinherit | 1}, 0, true, fsxattr{xflags: 1}},
		{"untag a file", fsxattr{projid: 7}, 0, false, fsxattr{}},
	} {
		if got := projectAttrs(tc.in, tc.pid, tc.dir); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A tenant container created before the seccomp profile can still re-tag its
// tree, at any depth and file by file; the check reads every entry and
// re-tags when one left the server's project or a directory lost inheritance.
func TestTreeTagsMoved(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"lobby/world/region", "survival", backupDirName} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"eula.txt", "lobby/world/region/r.0.0.mca", backupDirName + "/a.tar.gz"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tagged := fsxattr{projid: 7, xflags: fsXflagProjinherit}
	for _, tc := range []struct {
		name  string
		attrs map[string]fsxattr // by path below root, "" is root
		want  bool
	}{
		{"all tagged", nil, false},
		{"root untagged", map[string]fsxattr{"": {}}, true},
		{"a sub-server moved to project 0", map[string]fsxattr{"survival": {xflags: fsXflagProjinherit}}, true},
		{"a sub-server lost inheritance", map[string]fsxattr{"lobby": {projid: 7}}, true},
		// R53-06/R53-08: below the sub-server dir, and files too.
		{"deeper dir untagged", map[string]fsxattr{"lobby/world": {}}, true},
		{"deeper dir lost inheritance", map[string]fsxattr{"lobby/world/region": {projid: 7}}, true},
		{"file at the root untagged", map[string]fsxattr{"eula.txt": {}}, true},
		{"deep file untagged", map[string]fsxattr{"lobby/world/region/r.0.0.mca": {}}, true},
		// A file carries no inheritance flag to lose.
		{"file without inheritance", map[string]fsxattr{"eula.txt": {projid: 7}}, false},
		// The backup folder has its own budget.
		{"backup folder untagged", map[string]fsxattr{backupDirName: {}, backupDirName + "/a.tar.gz": {}}, false},
	} {
		byIno := map[uint64]fsxattr{}
		for p, fa := range tc.attrs {
			byIno[inodeOf(t, filepath.Join(root, p))] = fa
		}
		moved, err := treeTagsMoved(root, backupDirName, 7, func(fd int) (fsxattr, error) {
			var st syscall.Stat_t
			if err := syscall.Fstat(fd, &st); err != nil {
				return fsxattr{}, err
			}
			if fa, ok := byIno[st.Ino]; ok {
				return fa, nil
			}
			return tagged, nil
		})
		if err != nil || moved != tc.want {
			t.Errorf("%s: moved = %v, %v; want %v", tc.name, moved, err, tc.want)
		}
	}
}

// R53-03: the backup folder walk decides per entry on the real inode, a hard
// link to a world file included, and the check sees a tenant file left
// uncharged.
func TestBackupTreeProjects(t *testing.T) {
	const pid = 7
	root := t.TempDir()
	dir := filepath.Join(root, backupDirName)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"world.dat", backupDirName + "/a.tar.gz", backupDirName + "/a.tar.gz.partial", backupDirName + "/r.0.0.mca", backupDirName + "/sub/c.tar.gz"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(root, "world.dat"), filepath.Join(dir, "w.tar.gz")); err != nil {
		t.Fatal(err)
	}
	want := map[string]uint32{"": 0, "a.tar.gz": 0, "a.tar.gz.partial": 0, "r.0.0.mca": pid, "w.tar.gz": pid, "sub": pid, "sub/c.tar.gz": pid}
	for _, share := range []bool{false, true} {
		got := map[uint64]uint32{}
		if err := walkProjectTreeAt(dir, "", func(fd int, name string, depth int, isDir bool) error {
			p, err := backupFdProject(fd, name, depth, isDir, pid, share)
			var st syscall.Stat_t
			if serr := syscall.Fstat(fd, &st); serr != nil {
				return serr
			}
			got[st.Ino] = p
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for p, w := range want {
			if share {
				w = pid
			}
			if g := got[inodeOf(t, filepath.Join(dir, p))]; g != w {
				t.Errorf("share=%v %q: project %d, want %d", share, p, g, w)
			}
		}
	}

	byIno := map[uint64]fsxattr{}
	for p, w := range want {
		fa := fsxattr{projid: w}
		if w != 0 && (p == "" || p == "sub") {
			fa.xflags = fsXflagProjinherit
		}
		byIno[inodeOf(t, filepath.Join(dir, p))] = fa
	}
	get := func(fd int) (fsxattr, error) {
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			return fsxattr{}, err
		}
		return byIno[st.Ino], nil
	}
	if moved, err := backupTreeTagsMoved(dir, pid, false, get); err != nil || moved {
		t.Errorf("as tagged: moved = %v, %v; want false", moved, err)
	}
	byIno[inodeOf(t, filepath.Join(dir, "r.0.0.mca"))] = fsxattr{}
	if moved, err := backupTreeTagsMoved(dir, pid, false, get); err != nil || !moved {
		t.Errorf("tenant file uncharged: moved = %v, %v; want true", moved, err)
	}
}
