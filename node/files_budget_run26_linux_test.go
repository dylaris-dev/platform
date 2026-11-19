package main

import (
	"os"
	"path/filepath"
	"testing"
)

// R26-05: a failed copy removed the folder it made by name through a Root at
// the server directory, which follows a link a tenant swapped in for a parent
// meanwhile (survival/x -> ../.dylaris-backups/job-1), so it unlinked the
// node-local backup of the same name.
func TestAFailedCopyRemovesOnlyTheFolderItMade(t *testing.T) {
	for _, tc := range []struct {
		name     string
		swap     bool
		wantGone bool // the made folder is removed
	}{
		{"untouched", false, true},
		{"parent swapped for a link", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, root := seedNodeOwned(t)
			made := filepath.Join(root, "survival", "x", "a.tar.gz")
			if err := os.MkdirAll(made, 0o755); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(made)
			if err != nil {
				t.Fatal(err)
			}
			if tc.swap {
				if err := os.Rename(filepath.Join(root, "survival", "x"), filepath.Join(root, "survival", "y")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../.dylaris-backups/job-1", filepath.Join(root, "survival", "x")); err != nil {
					t.Fatal(err)
				}
			}
			removeMadeDir(root, "survival/x/a.tar.gz", info)
			if _, err := os.Stat(filepath.Join(root, ".dylaris-backups", "job-1", "a.tar.gz")); err != nil {
				t.Errorf("the node-local backup is gone: %v", err)
			}
			if _, err := os.Lstat(made); !tc.swap && os.IsNotExist(err) != tc.wantGone {
				t.Errorf("made folder gone = %v, want %v", os.IsNotExist(err), tc.wantGone)
			}
		})
	}
}
