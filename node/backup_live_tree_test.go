package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// A symlink inside the server is archived as its target while the size
// estimate does not follow it, so the spool file outgrew its reservation and
// the writes beside it never saw the difference. The file may now hold no more
// than what is held in inflightWrites for it.
func TestSpoolFileIsBoundByItsReservation(t *testing.T) {
	const gb = 1_000_000_000
	on := &backupPolicy{SpoolEnabled: true, SpoolMaxGB: 100}
	t.Cleanup(func() { inflightWrites.Store(0) })
	tests := []struct {
		name string
		src  int64
		free uint64
	}{
		{"small server on a big disk", 1 * gb, 500 * gb},
		{"large server", 40 * gb, 60 * gb},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inflightWrites.Store(0)
			limit, release, why := reserveSpool(on, tt.src, tt.free)
			defer release(0)
			if limit <= 0 {
				t.Fatalf("refused: %s", why)
			}
			if held := inflightWrites.Load(); limit > held {
				t.Fatalf("spool file may grow to %d, only %d is reserved for it", limit, held)
			}
			if limit < tt.src {
				t.Fatalf("limit %d is below the source size %d", limit, tt.src)
			}
		})
	}
}

// An archive that outgrows its reservation uploads directly instead of
// failing the run, and leaves no spool file behind.
func TestRunBackupFallsBackWhenTheSpoolIsOutgrown(t *testing.T) {
	spool := t.TempDir()
	prevDir, prevPause, prevPolicy, prevSlack := globalSpoolDir, pauseSaves, backupPolicyNow.Load(), spoolSlackBytes
	t.Cleanup(func() {
		globalSpoolDir, pauseSaves, spoolSlackBytes = prevDir, prevPause, prevSlack
		backupPolicyNow.Store(prevPolicy)
		inflightWrites.Store(0)
	})
	globalSpoolDir = spool
	backupPolicyNow.Store(&backupPolicy{HeadroomPct: i64(30), SpoolEnabled: true, SpoolMaxGB: 1})
	// The fixture's 5000 random bytes reserve about 100 bytes: the archive
	// outgrows it the way a tree of in-server links outgrows du.
	spoolSlackBytes = -4900

	core := newFakeCore(t, 1024)
	pauseSaves = func(_ context.Context, _ *redis.Client, _ *DockerManager, uuid, _ string) *worldSaveGuard {
		return &worldSaveGuard{uuid: uuid, send: func(string) error { return nil }}
	}
	if report := runBackupWithFakeCore(t, core, nil); report["status"] != "success" {
		t.Fatalf("report = %v, want success", report)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Fatalf("spool left %v behind", entries)
	}
	if got := inflightWrites.Load(); got != 0 {
		t.Fatalf("inflightWrites = %d after the run, want 0", got)
	}
}

// deleteOnFirstWrite removes path the first time the archive writes anything,
// which is the first tar header: after the walk took the file's Info and
// before the file was opened.
type deleteOnFirstWrite struct {
	bytes.Buffer
	path string
	done bool
}

func (d *deleteOnFirstWrite) Write(p []byte) (int, error) {
	if !d.done {
		d.done = true
		os.Remove(d.path)
	}
	return d.Buffer.Write(p)
}

// A plugin's short-lived file removed while its header was being written
// failed the whole backup with "no such file".
func TestArchiveSurvivesAFileRemovedAfterItWasListed(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.journal", "b.dat"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w := &deleteOnFirstWrite{path: filepath.Join(root, "a.journal")}
	if _, err := writeServerArchive(w, root, root, nil, nil, nil); err != nil {
		t.Fatalf("archive = %v, want the vanished file tolerated", err)
	}
	if got := readArchive(t, w.Bytes()); got["b.dat"] != "b.dat" {
		t.Fatalf("archive = %v, want b.dat in it", got)
	}
}

// save-on was pushed fire-and-forget: a failed push left the server with
// saving off until its next restart and logged "saving resumed".
func TestWorldSaveGuardRetriesSaveOn(t *testing.T) {
	prev := saveOnRetryDelay
	saveOnRetryDelay = time.Millisecond
	t.Cleanup(func() { saveOnRetryDelay = prev })
	tests := []struct {
		name      string
		failFirst int
		wantCalls int
	}{
		{"first push lands", 0, 1},
		{"a blip is retried", 2, 3},
		{"gives up after its attempts", 100, saveOnAttempts},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			g := &worldSaveGuard{uuid: "srv-1", send: func(c string) error {
				if c != "save-on" {
					t.Fatalf("sent %q", c)
				}
				calls++
				if calls <= tt.failFirst {
					return errors.New("redis down")
				}
				return nil
			}}
			g.resume()
			g.resume()
			if calls != tt.wantCalls {
				t.Fatalf("save-on pushed %d times, want %d", calls, tt.wantCalls)
			}
		})
	}
}
