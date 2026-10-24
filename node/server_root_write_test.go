package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A link planted in the server's directory (the tenant's /data) must not carry
// the node's write out of it.
func TestWriteServerRootFileStaysInTheServerDirectory(t *testing.T) {
	base := t.TempDir()
	srv := filepath.Join(base, "srv")
	if err := os.Mkdir(srv, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "victim")
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(srv, ".node_config.json")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if err := writeServerRootFile(srv, ".node_config.json", []byte("{}")); err == nil {
		t.Fatal("the write followed a link out of the server directory")
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatalf("the file outside was overwritten: %q", b)
	}

	// A plain file is written as before.
	plain := filepath.Join(base, "plain")
	os.Mkdir(plain, 0o755)
	if err := writeServerRootFile(plain, ".node_config.json", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(plain, ".node_config.json")); string(b) != `{"a":1}` {
		t.Fatalf("got %q", b)
	}
}
