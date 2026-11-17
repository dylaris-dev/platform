package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// V09: renameNoFollow staged a top-level directory in the tenant's survival/,
// and both of its roll-backs moved whatever sat at the staging name back to
// the server's root-owned top level. The tenant swaps it for a link in that
// window and gets a link to another server's tree under a sub-server name.
func TestRenameNoFollowRollbackDoesNotBringBackASwappedLink(t *testing.T) {
	tests := []struct {
		name      string
		occupied  bool // survival/lobby is a non-empty directory, so the final rename fails
		swap      bool
		judged    bool // swap after the link check instead of before it
		wantBack  bool // lobby is back at the top as the real directory
		wantError bool
	}{
		{"swap before the judgement", false, true, false, false, true},
		{"swap before a failed final rename", true, true, true, false, true},
		{"failed final rename, no swap", true, false, false, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			victim := t.TempDir()
			for _, d := range []string{"lobby", "survival"} {
				if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "lobby", "server.jar"), []byte("jar"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.occupied {
				if err := os.MkdirAll(filepath.Join(root, "survival", "lobby", "x"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.swap {
				renameStagedHook = func(nd *os.File, tmp string, judged bool) {
					if judged != tc.judged {
						return
					}
					if err := unix.Renameat(int(nd.Fd()), tmp, int(nd.Fd()), "stolen"); err != nil {
						t.Errorf("swap: %v", err)
					}
					if err := unix.Symlinkat(victim, int(nd.Fd()), tmp); err != nil {
						t.Errorf("plant: %v", err)
					}
				}
				t.Cleanup(func() { renameStagedHook = nil })
			}

			err := renameNoFollow(root, "lobby", "survival/lobby")
			if (err != nil) != tc.wantError {
				t.Fatalf("err = %v, want error %v", err, tc.wantError)
			}
			fi, lerr := os.Lstat(filepath.Join(root, "lobby"))
			if lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("a link landed at the server's top level as lobby")
			}
			if back := lerr == nil && fi.IsDir(); back != tc.wantBack {
				t.Fatalf("lobby back at the top = %v, want %v", back, tc.wantBack)
			}
			if tc.wantBack {
				if _, err := os.Stat(filepath.Join(root, "lobby", "server.jar")); err != nil {
					t.Fatalf("the restored lobby lost its content: %v", err)
				}
			}
			entries, _ := os.ReadDir(root)
			for _, e := range entries {
				if fi, _ := e.Info(); fi != nil && fi.Mode()&os.ModeSymlink != 0 {
					t.Errorf("link %s left at the server's top level", e.Name())
				}
			}
		})
	}
}
