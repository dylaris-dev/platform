package main

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A named pipe where an archive is expected is refused at once instead of
// blocking the installer forever.
func TestArchiveOpenRefusesANamedPipe(t *testing.T) {
	dir := t.TempDir()
	pipe := filepath.Join(dir, ".upload.zip")
	if err := unix.Mkfifo(pipe, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 2)
	go func() {
		f, err := openNoFollow(pipe)
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	go func() {
		_, err := uploadTopFolder(pipe)
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a named pipe was opened as an archive")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("opening a named pipe blocked")
		}
	}
}
