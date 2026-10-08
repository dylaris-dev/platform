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

// The store is skipped by name, but a symlink elsewhere onto one of its
// archives was archived as its target: every run carried every earlier one.
func TestABackupDoesNotFollowALinkIntoTheArchiveStore(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, backupDirName), 0o755)
	os.MkdirAll(filepath.Join(root, "world"), 0o755)
	os.WriteFile(filepath.Join(root, backupDirName, "old.tar.gz"), []byte("an earlier archive"), 0o644)
	os.WriteFile(filepath.Join(root, "world", "level.dat"), []byte("world"), 0o644)
	if err := os.Symlink("../"+backupDirName+"/old.tar.gz", filepath.Join(root, "world", "linked")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if _, err := writeServerArchive(&buf, root, root, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	gr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gr)
	names := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
	}
	if names["world/linked"] {
		t.Error("a link into the archive store was archived as the archive")
	}
	if !names["world/level.dat"] {
		t.Errorf("the world itself is missing: %v", names)
	}
}

// A link INSIDE the store onto a world file must not make that file look like
// an archive: it would leave every backup without the world.
func TestALinkInTheArchiveStoreDoesNotHideTheWorld(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, backupDirName), 0o755)
	os.MkdirAll(filepath.Join(root, "world"), 0o755)
	os.WriteFile(filepath.Join(root, "world", "level.dat"), []byte("world"), 0o644)
	if err := os.Symlink("../world/level.dat", filepath.Join(root, backupDirName, "trap")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := writeServerArchive(&buf, root, root, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	gr, _ := gzip.NewReader(&buf)
	tr := tar.NewReader(gr)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == "world/level.dat" {
			return
		}
	}
	t.Error("a link in the archive store kept the world out of the backup")
}
