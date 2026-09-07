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

// entryNames reads an archive produced by writeServerArchive, in order.
func entryNames(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	gr, err := gzip.NewReader(buf)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	var out []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		out = append(out, hdr.Name)
	}
	return out
}

func seedServer(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "world", "level.dat"), []byte("w"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// The manifest has to be the FIRST entry, because a reader that only wants the
// description must not have to stream a multi-gigabyte world to reach it.
func TestWriteServerArchive_ManifestIsTheFirstEntry(t *testing.T) {
	root := seedServer(t)
	var buf bytes.Buffer
	added, err := writeServerArchive(&buf, root, root, nil, nil, []byte(`{"schema":1}`))
	if err != nil {
		t.Fatalf("writeServerArchive: %v", err)
	}
	if !added {
		t.Fatal("added = false, want true: the world file should have been archived")
	}
	names := entryNames(t, &buf)
	if len(names) == 0 || names[0] != manifestEntryName {
		t.Fatalf("entries = %v, want %q first", names, manifestEntryName)
	}
}

// An older Core sends no manifest. That must produce exactly the archive it
// always produced, not one with an empty description in it.
func TestWriteServerArchive_NoManifestWritesNoEntry(t *testing.T) {
	root := seedServer(t)
	var buf bytes.Buffer
	if _, err := writeServerArchive(&buf, root, root, nil, nil, nil); err != nil {
		t.Fatalf("writeServerArchive: %v", err)
	}
	for _, n := range entryNames(t, &buf) {
		if isManifestEntry(n) {
			t.Errorf("archive contains %q with no manifest supplied", n)
		}
	}
}

// A manifest alone is not a backup. A run whose patterns match nothing must
// still report "nothing matched" rather than upload an archive of its own
// description - the caller decides on `added`, and the manifest must not set it.
func TestWriteServerArchive_ManifestDoesNotCountAsContent(t *testing.T) {
	root := seedServer(t)
	var buf bytes.Buffer
	added, err := writeServerArchive(&buf, root, root, []string{"nothing-matches-this/**"}, nil, []byte(`{"schema":1}`))
	if err != nil {
		t.Fatalf("writeServerArchive: %v", err)
	}
	if added {
		t.Error("added = true with no file matched; the manifest was counted as content")
	}
}

// The reserved directory must not be archived out of a live server tree. If it
// ever lands there, the next backup would carry a manifest describing a
// different backup, nested inside the new one.
func TestWriteServerArchive_SkipsAStrayManifestDirOnDisk(t *testing.T) {
	root := seedServer(t)
	if err := os.MkdirAll(filepath.Join(root, manifestDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, manifestDirName, "backup.json"), []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := writeServerArchive(&buf, root, root, nil, nil, nil); err != nil {
		t.Fatalf("writeServerArchive: %v", err)
	}
	for _, n := range entryNames(t, &buf) {
		if isManifestEntry(n) {
			t.Errorf("archive contains %q from the server tree; the walk must skip it", n)
		}
	}
}

// The restore path drops these entries by name, and a tar header is written by
// whoever produced the archive - so the check has to survive the spellings a
// producer may use rather than only the one this node writes.
func TestIsManifestEntry(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{".dylaris/backup.json", true},
		{"./.dylaris/backup.json", true},
		{".dylaris", true},
		{".dylaris/anything/deeper.json", true},
		{"world/level.dat", false},
		{"mods/sodium.jar", false},
		// Not the reserved directory: a server may legitimately hold these.
		{".dylaris-backups/x.tar.gz", false},
		{"config/.dylaris/keep.txt", false},
	}
	for _, tt := range tests {
		if got := isManifestEntry(tt.name); got != tt.want {
			t.Errorf("isManifestEntry(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
