package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"dylaris-pkg/fileperms"

	"github.com/pkg/sftp"
)

// H40: the move fence only holds back lookups made after it. An SFTP handle
// opened before it kept writing into the source tree, which the move then
// deleted, and the client saw a successful upload of a truncated file. A move
// is refused while a handle is open for writing and goes through once it
// closed.
func TestAStorageMoveWaitsForNoOpenSFTPWriter(t *testing.T) {
	sm, paths := newPlacementManager(t, 2)
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7"
	if err := os.MkdirAll(filepath.Join(paths[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
	w, err := newVirtualFS(refs, sm, nil, "alice").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/world.zip", Flags: 0x02 | 0x08 | 0x10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt([]byte("half"), 0); err != nil {
		t.Fatal(err)
	}

	if err := sm.MigrateServerPath(uuid, paths[1]); err == nil {
		t.Fatal("the move ran under an open SFTP write handle")
	}
	if _, err := os.Stat(filepath.Join(paths[0], uuid, "world.zip")); err != nil {
		t.Fatalf("the refused move touched the source: %v", err)
	}

	if err := w.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	if err := sm.MigrateServerPath(uuid, paths[1]); err != nil {
		t.Fatalf("the move after the handle closed: %v", err)
	}
}
