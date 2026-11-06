package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The temp file is created next to the upload's DESTINATION, and
// validateBeamPath puts no depth limit on that. The sweeper used to glob the
// server dir and one level below it, on the note that "one level deep is
// enough" - so an upload into "survival/plugins/BlueMap/" left its partial
// file where nothing ever looked, and a kill mid-stream kept it on the
// server's disk for good, counting against its limit.
//
// Every depth here is a real upload target, not a synthetic one: a world
// folder, a plugin's own config dir, a datapack.
func TestTheUploadSweeperFindsTempsAtEveryDepth(t *testing.T) {
	root := t.TempDir()
	sm := NewStorageManager(root, nil)
	serverDir := filepath.Join(root, "srv-uuid")

	stale := []string{
		".beam-upload-atroot",
		filepath.Join("survival", ".beam-upload-subserver"),
		filepath.Join("survival", "plugins", ".beam-upload-plugins"),
		filepath.Join("survival", "plugins", "BlueMap", ".beam-upload-deep"),
		filepath.Join("survival", "world", "datapacks", "pack", "data", ".beam-upload-deeper"),
	}
	for _, rel := range stale {
		p := filepath.Join(serverDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Older than the grace period, which is what "the writer is gone" means.
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	// A live upload and an ordinary file the tenant owns. Neither may be touched:
	// the first is still being written, the second is not ours to delete.
	live := filepath.Join(serverDir, "survival", "plugins", ".beam-upload-live")
	if err := os.WriteFile(live, []byte("writing"), 0o644); err != nil {
		t.Fatal(err)
	}
	theirs := filepath.Join(serverDir, "survival", "plugins", "BlueMap", "config.conf")
	if err := os.WriteFile(theirs, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(theirs, old, old); err != nil {
		t.Fatal(err)
	}

	// An already-cancelled context runs the immediate sweep and returns before
	// the ticker, so the sweep is exercised without waiting for one.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sweepStaleUploadTemps(ctx, sm, nil)

	for _, rel := range stale {
		if _, err := os.Stat(filepath.Join(serverDir, rel)); err == nil {
			t.Errorf("stale temp %s survived the sweep, so it stays on the server's disk for good", rel)
		}
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("the sweeper deleted a temp that is still being written: %v", err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("the sweeper deleted a file that is not ours: %v", err)
	}
}
