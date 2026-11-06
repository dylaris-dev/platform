package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// readServerRootFile checked the name with Lstat and then opened it blocking,
// so a root container swapping a pipe in between held the read - the node's
// boot (restorePortLedger) or a start - until a writer appeared, which is never.
func TestServerRootReadsDoNotBlockOnASwappedInPipe(t *testing.T) {
	srv := t.TempDir()
	name := filepath.Join(srv, ".node_config.json")
	var stop atomic.Bool
	swapped := make(chan struct{})
	go func() {
		defer close(swapped)
		for !stop.Load() {
			os.Remove(name)
			os.WriteFile(name, []byte(`{}`), 0o600)
			os.Remove(name)
			unix.Mkfifo(name, 0o600)
		}
	}()
	defer func() { stop.Store(true); <-swapped }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		done := make(chan struct{})
		go func() { readServerRootFile(srv, ".node_config.json"); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			stop.Store(true)
			// Let the stuck open return so the test can end.
			if f, err := os.OpenFile(name, os.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
				f.Close()
			}
			t.Fatal("readServerRootFile blocked on a pipe swapped in after its Lstat")
		}
	}
}
