//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// An inode number names a file only while something keeps it allocated. The
// walk closes each file once archived, so an inode freed during the walk (a
// plugin renaming a temp file over its data, retention deleting an archive)
// could come back under another name. Reuse is simulated by moving the inode
// to a later name and rewriting it in place: the walk sees the identity it
// recorded earlier, under a name that no longer is that file.
func TestAnInodeReusedDuringTheWalkIsArchivedInFull(t *testing.T) {
	tests := []struct {
		name string
		// first is the earlier holder of the inode, as a path under the root.
		first string
		// atRead is the file read when the inode moves: m.yml's, after
		// a.yml is archived and closed.
		atRead int
	}{
		{"a file archived earlier in the walk", "a.yml", 2},
		{"an archive in the store", filepath.Join(backupDirName, "old.tar.gz"), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			os.MkdirAll(filepath.Join(root, backupDirName), 0o755)
			os.MkdirAll(filepath.Join(root, "zz"), 0o755)
			first := filepath.Join(root, tc.first)
			os.WriteFile(first, []byte("old content of the first holder"), 0o644)
			os.WriteFile(filepath.Join(root, "m.yml"), []byte("middle"), 0o644)
			reused := filepath.Join(root, "zz", "store.yml")

			calls := 0
			pace := func(r io.Reader) io.Reader {
				calls++
				if calls == tc.atRead {
					if err := os.Rename(first, reused); err != nil {
						t.Fatal(err)
					}
					if tc.first == "a.yml" {
						os.WriteFile(first, []byte("a fresh a.yml"), 0o644)
					}
					f, err := os.OpenFile(reused, os.O_WRONLY|os.O_TRUNC, 0)
					if err != nil {
						t.Fatal(err)
					}
					f.WriteString("Z's own data")
					f.Close()
				}
				return r
			}
			var buf bytes.Buffer
			if _, err := writeServerArchivePaced(&buf, root, root, nil, nil, nil, pace); err != nil {
				t.Fatal(err)
			}
			gr, err := gzip.NewReader(&buf)
			if err != nil {
				t.Fatal(err)
			}
			tr := tar.NewReader(gr)
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if h.Name != "zz/store.yml" {
					continue
				}
				if h.Typeflag != tar.TypeReg {
					t.Fatalf("zz/store.yml archived as type %q linking %q, want its own bytes", h.Typeflag, h.Linkname)
				}
				body, _ := io.ReadAll(tr)
				if string(body) != "Z's own data" {
					t.Fatalf("zz/store.yml = %q", body)
				}
				return
			}
			t.Fatal("zz/store.yml is missing from the archive")
		})
	}
}
