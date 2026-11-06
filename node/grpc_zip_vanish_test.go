package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Minecraft writes level.dat and playerdata through a temp file it renames
// away. One the walk listed but that is gone by the time it is opened used to
// abort the whole folder download of a running server.
func TestAddZipEntrySkipsAFileThatVanished(t *testing.T) {
	cases := []struct {
		name      string
		vanish    bool
		wantNames []string
	}{
		{"present file is archived", false, []string{"level.dat_tmp"}},
		{"vanished file is skipped", true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "level.dat_tmp")
			if err := os.WriteFile(p, []byte("nbt"), 0644); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			if c.vanish {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()

			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			if err := addZipEntry(zw, root, "level.dat_tmp", "level.dat_tmp", info, nil); err != nil {
				t.Fatalf("addZipEntry: %v", err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, f := range zr.File {
				got = append(got, f.Name)
			}
			if len(got) != len(c.wantNames) || (len(got) == 1 && got[0] != c.wantNames[0]) {
				t.Errorf("entries = %v, want %v", got, c.wantNames)
			}
		})
	}
}
