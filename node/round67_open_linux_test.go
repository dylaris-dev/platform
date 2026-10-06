package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A walk's read-open refuses a pipe at once and hands back a regular file in
// blocking mode; openNoFollow too.
func TestRegularOpensRefusePipesAndStayBlocking(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "world.dat"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "level.dat"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	done := make(chan error, 1)
	go func() {
		f, err := openRegularIn(root, "world.dat")
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a pipe was opened as a regular file")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a pipe through the root blocked")
	}

	for name, open := range map[string]func() (*os.File, error){
		"openRegularIn": func() (*os.File, error) { return openRegularIn(root, "level.dat") },
		"openNoFollow":  func() (*os.File, error) { return openNoFollow(filepath.Join(dir, "level.dat")) },
	} {
		f, err := open()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Through SyscallConn: f.Fd() would switch the descriptor to blocking
		// itself and hide the very flag this looks for.
		var fl int
		rc, err := f.SyscallConn()
		if err == nil {
			cerr := rc.Control(func(fd uintptr) { fl, err = unix.FcntlInt(fd, unix.F_GETFL, 0) })
			if err == nil {
				err = cerr
			}
		}
		f.Close()
		if err != nil || fl&unix.O_NONBLOCK != 0 {
			t.Errorf("%s left the file non-blocking (flags %#x, %v)", name, fl, err)
		}
	}
}

// The backup archive writer, the tenant copy and the modpack index read open
// through the regular-file helpers.
func TestWalkReadsOpenOnlyRegularFiles(t *testing.T) {
	for file, want := range map[string]string{
		"backup_worker.go":     "f, err := openRegularIn(root, name)",
		"rootfs.go":            "in, err := openRegularIn(src, srcName)",
		"installer_modpack.go": "zf, err := openNoFollow(path)",
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%s no longer opens with %q", file, want)
		}
	}
}
