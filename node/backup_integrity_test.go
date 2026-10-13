package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The archive waited a fixed five seconds after "save-all flush". A large
// modded world takes longer, and a tar reading region files mid-write is a
// damaged backup nobody notices until a restore. It now waits for the server's
// own confirmation - and not for an earlier save's.
func TestABackupWaitsForTheServerToConfirmTheSave(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	key := serverLogStreamKey("u1", "survival")
	add := func(line string) string {
		id, _ := rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]interface{}{"line": line}}).Result()
		return id
	}
	from := add("[Server thread/INFO]: Saved the game") // an earlier save

	if waitForSaveConfirmation(ctx, rdb, key, from, 300*time.Millisecond) {
		t.Fatal("an earlier save's confirmation was read as this one's")
	}
	// The announcement is not the confirmation.
	add("[Server thread/INFO]: Saving the game (this may take a moment!)")
	if waitForSaveConfirmation(ctx, rdb, key, from, 300*time.Millisecond) {
		t.Fatal("\"Saving the game\" was taken for the confirmation")
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		add("[Server thread/INFO]: Saving the game (this may take a moment!)")
		add("[Server thread/INFO]: Saved the game")
	}()
	start := time.Now()
	if !waitForSaveConfirmation(ctx, rdb, key, from, 5*time.Second) {
		t.Fatal("the confirmation was not seen")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("it waited past the confirmation")
	}
}

// A failure that only shows on sync or close used to be dropped, and the
// truncated archive was recorded as a good backup.
func TestTheArchiveIsCommittedOrFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyAndCommit(f, strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "archive" {
		t.Fatalf("wrote %q", b)
	}
	if err := copyAndCommit(f, strings.NewReader("again")); err == nil {
		t.Fatal("a write to a closed archive reported success")
	}
}

// The wiring, named per site.
func TestBackupAndRestoreWiring(t *testing.T) {
	read := func(f string) string {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	worker, restore, mainSrc := read("backup_worker.go"), read("backup_restore.go"), read("main.go")
	if strings.Contains(worker, "time.Sleep(saveFlushWait)") || !strings.Contains(worker, "waitForSaveConfirmation(ctx, rdb, logKey, from, saveFlushTimeout)") {
		t.Error("the backup no longer waits for the save confirmation")
	}
	if !strings.Contains(worker, "if err := f.Close(); err != nil {\n\t\treturn fmt.Errorf(\"close the archive: %w\", err)") {
		t.Error("the archive's close error is dropped again")
	}
	if strings.Count(worker, "copyAndCommit(") < 3 {
		t.Error("an archive write drops its sync or close error")
	}
	if !strings.Contains(worker, "budget := &writeBudget{left: restoreDiskBudget(dir) - inflightWrites.Load()") {
		t.Error("a node-local archive is written with no disk budget")
	}
	if !strings.Contains(restore, "budget := restoreDiskBudget(filepath.Dir(stageDir)) - inflightWrites.Load()") ||
		!strings.Contains(restore, "inflightWrites.Add(n)") {
		t.Error("a restore no longer shares the disk with the other writes")
	}
	if !strings.Contains(mainSrc, "if !restoreServersInFlight.has(rcmd.ServerUUID) {\n\t\t\treleaseRestoreBusy := holdBusyStatus(") {
		t.Error("a refused second restore releases the running restore's interlock")
	}
}

// Older servers confirm with "Saved the world"; a console that does not know
// the save commands says so, and there is then nothing to wait for.
func TestSaveConfirmationAcrossVersions(t *testing.T) {
	for line, want := range map[string]bool{
		"[Server thread/INFO]: Saved the game":                              true,
		"[Server thread/INFO] [minecraft/DedicatedServer]: Saved the world": true,
		"[Server thread/INFO]: Saving the game (this may take a moment!)":   false,
	} {
		if saveConfirmed(line) != want {
			t.Errorf("%q: confirmed=%v, want %v", line, !want, want)
		}
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	key := serverLogStreamKey("u2", "")
	rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]interface{}{"line": "Unknown command. Type \"/help\" for help."}})
	start := time.Now()
	if waitForSaveConfirmation(ctx, rdb, key, "0", 5*time.Second) || time.Since(start) > 2*time.Second {
		t.Fatal("waited on a console that does not know the save commands")
	}
}
