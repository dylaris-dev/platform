package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A second name for the node's config inside a sub-server took it into that
// sub-server's archive.
func TestAnArchiveLeavesOutTheNodesConfigUnderAnotherName(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, ".node_config.json")
	if err := os.WriteFile(cfg, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main", "world.dat"), []byte("w"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(cfg, filepath.Join(root, "main", "hard")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.node_config.json", filepath.Join(root, "main", "soft")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := writeServerArchive(&buf, root, filepath.Join(root, "main"), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := readArchive(t, buf.Bytes())
	if _, ok := got["world.dat"]; !ok || len(got) != 1 {
		t.Fatalf("archive = %v, want only world.dat", got)
	}
}
