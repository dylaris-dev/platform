package main

import (
	"dylaris-pkg/fileperms"

	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pkg/sftp"
)

// SFTP builds its own paths instead of going through resolveWithinDir, so it
// needed the symlink half of that guard restated. Clean("/"+path) strips
// traversal from the STRING; os.Open then follows a planted link out of the
// server directory regardless.
//
// SFTP itself cannot CREATE a link (Symlink is not an implemented method), but
// the same directory is bind-mounted into the tenant's Minecraft container,
// which can.
func TestSFTPDoesNotFollowALinkOutOfTheServerDirectory(t *testing.T) {
	sm, _ := newPlacementManager(t, 1)
	const uuid = "srv-uuid"
	base := filepath.Join(sm.Paths()[0], uuid)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "server.properties"), []byte("motd=hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	secret := filepath.Join(t.TempDir(), "node_secret")
	if err := os.WriteFile(secret, []byte("NODE SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(base, "escape.txt")); err != nil {
		if runtime.GOOS == "windows" || errors.Is(err, os.ErrPermission) {
			t.Skipf("cannot create symlinks here: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.Symlink("server.properties", filepath.Join(base, "inside.txt")); err != nil {
		t.Fatal(err)
	}

	fs := newVirtualFS([]sftpServerRef{{UUID: uuid, Name: "myserver", Perms: fileperms.Full()}}, sm, nil, "tester")

	if _, err := fs.Fileread(&sftp.Request{Method: "Get", Filepath: "myserver/escape.txt"}); err == nil {
		t.Error("downloading a link that leaves the server directory was accepted")
	}
	// The link's target must not become writable either: an open with O_CREAT
	// follows the link and truncates whatever it names.
	if _, err := fs.Filewrite(&sftp.Request{Method: "Put", Filepath: "myserver/escape.txt"}); err == nil {
		t.Error("writing through a link that leaves the server directory was accepted")
	}
	if body, err := os.ReadFile(secret); err != nil || string(body) != "NODE SECRET" {
		t.Errorf("the file outside the server directory was modified: %q (err %v)", body, err)
	}

	// Ordinary files and contained links keep working.
	r, err := fs.Fileread(&sftp.Request{Method: "Get", Filepath: "myserver/inside.txt"})
	if err != nil {
		t.Fatalf("a contained link was refused: %v", err)
	}
	buf := make([]byte, 16)
	n, err := r.ReadAt(buf, 0)
	if n == 0 || (err != nil && err != io.EOF) {
		t.Fatalf("read contained link: n=%d err=%v", n, err)
	}
	if string(buf[:n]) != "motd=hi" {
		t.Errorf("contained link read %q, want %q", buf[:n], "motd=hi")
	}
}

// A dangling link is refused for the same reason a resolvable one is: an open
// with O_CREAT follows it and CREATES the target, so it is a write primitive
// aimed wherever its text points.
func TestSFTPRefusesADanglingLink(t *testing.T) {
	sm, _ := newPlacementManager(t, 1)
	const uuid = "srv-uuid"
	base := filepath.Join(sm.Paths()[0], uuid)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(t.TempDir(), "does-not-exist-yet")
	if err := os.Symlink(planted, filepath.Join(base, "trap.txt")); err != nil {
		if runtime.GOOS == "windows" || errors.Is(err, os.ErrPermission) {
			t.Skipf("cannot create symlinks here: %v", err)
		}
		t.Fatal(err)
	}

	fs := newVirtualFS([]sftpServerRef{{UUID: uuid, Name: "myserver", Perms: fileperms.Full()}}, sm, nil, "tester")
	if _, err := fs.Filewrite(&sftp.Request{Method: "Put", Filepath: "myserver/trap.txt"}); err == nil {
		t.Error("writing through a dangling link was accepted")
	}
	if _, err := os.Lstat(planted); err == nil {
		t.Errorf("the write created %s, outside the server directory", planted)
	}
}
