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

// R32-07: a copy's temp sits next to its destination as .copy-<hex>.tmp, and
// only the copy itself removed it, so a node restart mid-copy left it on the
// server's disk for good.
func TestTheUploadSweeperRemovesStaleCopyTemps(t *testing.T) {
	root := t.TempDir()
	sm := NewStorageManager(root, nil)
	serverDir := filepath.Join(root, "srv-uuid")
	for _, tc := range []struct {
		rel  string
		age  time.Duration
		gone bool
	}{
		{filepath.Join("survival", "world", ".copy-0123456789abcdef.tmp"), time.Hour, true},
		// A copy still running writes continuously; one a minute idle is not dead.
		{filepath.Join("survival", ".copy-fedcba9876543210.tmp"), time.Minute, false},
		// Not the copy's pattern: the tenant's own file.
		{filepath.Join("survival", ".copy-notes.txt"), time.Hour, false},
		{filepath.Join("survival", ".copy-notes.tmp"), time.Hour, false},
	} {
		p := filepath.Join(serverDir, tc.rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-tc.age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, err := os.Stat(p)
			if gone := os.IsNotExist(err); gone != tc.gone {
				t.Errorf("%s (%s old): removed %v, want %v", tc.rel, tc.age, gone, tc.gone)
			}
		}()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sweepStaleUploadTemps(ctx, sm, nil)
}

// R35-10: on storage another node also uses, a stale-looking temp may be that
// node's live, throttled upload; held() knows only this process's streams.
func TestTheUploadSweeperLeavesSharedStorageAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shared bool
		gone   bool
	}{
		{"own storage", false, true},
		{"shared with another node", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sm := NewStorageManager(root, nil)
			p := filepath.Join(root, "srv-uuid", ".beam-upload-peer")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
			if tc.shared {
				if err := writeBeacon(root, nodeBeacon{NodeID: "other-node", UpdatedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			sweepStaleUploadTemps(ctx, sm, nil)
			_, err := os.Stat(p)
			if gone := os.IsNotExist(err); gone != tc.gone {
				t.Errorf("temp removed %v, want %v", gone, tc.gone)
			}
		})
	}
}

// R38-04: the sweeper walks a tenant's tree every five minutes, so it takes the
// same depth cap as the boot sweep; a tenant's endless nesting must not pin or
// crash the node.
func TestTheUploadSweeperStopsAtTheDepthCap(t *testing.T) {
	prev := maxTenantWalkDepth
	maxTenantWalkDepth = 3
	t.Cleanup(func() { maxTenantWalkDepth = prev })
	root := t.TempDir()
	sm := NewStorageManager(root, nil)
	serverDir := filepath.Join(root, "srv-uuid")
	cases := []struct {
		rel  string
		gone bool
	}{
		{"a/a/.beam-upload-within", true},   // depth 3: within the cap
		{"a/a/a/.beam-upload-below", false}, // depth 4: not entered
	}
	old := time.Now().Add(-time.Hour)
	for _, tc := range cases {
		p := filepath.Join(serverDir, filepath.FromSlash(tc.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var heldKeys []string
	sweepStaleUploadTemps(ctx, sm, func(key string) bool { heldKeys = append(heldKeys, key); return false })
	for _, tc := range cases {
		_, err := os.Lstat(filepath.Join(serverDir, filepath.FromSlash(tc.rel)))
		if gone := os.IsNotExist(err); gone != tc.gone {
			t.Errorf("%s: gone=%v, want %v", tc.rel, gone, tc.gone)
		}
	}
	// The stream key is still the server-relative path.
	if len(heldKeys) != 1 || heldKeys[0] != "srv-uuid/a/a/.beam-upload-within" {
		t.Errorf("held() asked for %v", heldKeys)
	}
}
