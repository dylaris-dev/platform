package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"dylaris-pkg/fileperms"

	"github.com/pkg/sftp"
)

// R40-07: a cross-node move archives the tree and drops the archive when any
// write began meanwhile. A refused or no-op SFTP command took the writer hold
// before its permission check and counted as that write, so a read-only
// member's SETSTAT or refused delete threw away the move.
func TestARefusedOrNoOpSFTPCommandIsNotAWriteForAMove(t *testing.T) {
	const uuid = "4b0c7f3e-2a51-4e0d-9a3b-7c1d2e3f4a5b"
	for _, tc := range []struct {
		name  string
		perms fileperms.Perms
		req   *sftp.Request
	}{
		{"mkdir without write", fileperms.Perms{Read: true}, &sftp.Request{Method: "Mkdir", Filepath: "s/new"}},
		{"remove without delete", fileperms.Perms{Read: true, Write: true}, &sftp.Request{Method: "Remove", Filepath: "s/a.txt"}},
		{"rmdir without delete", fileperms.Perms{Read: true}, &sftp.Request{Method: "Rmdir", Filepath: "s/dir"}},
		{"rename without write", fileperms.Perms{Read: true, Delete: true}, &sftp.Request{Method: "Rename", Filepath: "s/a.txt", Target: "s/b.txt"}},
		{"truncate without write", fileperms.Perms{Read: true}, &sftp.Request{Method: "Setstat", Filepath: "s/a.txt", Flags: 0x01, Attrs: make([]byte, 8)}},
		{"setstat of times", fileperms.Full(), &sftp.Request{Method: "Setstat", Filepath: "s/a.txt", Flags: 0x08, Attrs: make([]byte, 8)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm, paths := newPlacementManager(t, 1)
			if err := os.MkdirAll(filepath.Join(paths[0], uuid, "dir"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(paths[0], uuid, "a.txt"), []byte("hi"), 0o644); err != nil {
				t.Fatal(err)
			}
			wrote, err := sm.watchWrites(uuid)
			if err != nil {
				t.Fatal(err)
			}
			newVirtualFS([]sftpServerRef{{UUID: uuid, Name: "s", Perms: tc.perms}}, sm, nil, "alice").Filecmd(tc.req)
			if wrote() {
				t.Fatal("a command that changed nothing counted as a write and would drop the move's archive")
			}
		})
	}
}

// R40-08: losing write access refused an open handle's writes but kept its
// writer hold until the client closed it, so an idle sshfs handle made
// migrate_out refuse with "writers in progress". The republish that withdraws
// the access now ends the hold, and the handle stays refused after.
func TestWithdrawnWriteAccessEndsAnIdleSFTPHandlesHold(t *testing.T) {
	const uuid = "9e8d7c6b-5a49-4382-a1b0-c9d8e7f6a5b4"
	sm, paths := newPlacementManager(t, 1)
	if err := os.MkdirAll(filepath.Join(paths[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	full := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
	v := newVirtualFS(full, sm, nil, "alice")
	w, err := v.Filewrite(&sftp.Request{Method: "Put", Filepath: "s/cfg.yml", Flags: 0x02 | 0x08})
	if err != nil {
		t.Fatal(err)
	}
	defer w.(io.Closer).Close()
	if _, err := sm.watchWrites(uuid); err == nil {
		t.Fatal("precondition: the open handle should hold the server")
	}

	republish(v.set, v.open, []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Perms{Read: true}}})
	if _, err := sm.watchWrites(uuid); err != nil {
		t.Fatalf("the idle handle still holds the server after its write access went: %v", err)
	}

	// Write comes back (the move rolled back): the handle no longer holds the
	// server, so it must not write unheld.
	republish(v.set, v.open, full)
	if _, err := w.WriteAt([]byte("x"), 0); err == nil {
		t.Fatal("a handle whose hold was ended wrote again")
	}
}
