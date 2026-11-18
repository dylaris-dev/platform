package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/redis/go-redis/v9"
)

// R25-04: a server that was stopped when its backup began had no save-off
// window at all, so a start in the middle of an unspooled walk had the JVM
// write region files while tar read them. A start during the walk now pauses
// saving through the run's own guard, and the run's end turns it back on.
func TestAStartDuringABackupOfAStoppedServerPausesSaving(t *testing.T) {
	prevPause, prevPolicy := pauseSaves, backupPolicyNow.Load()
	t.Cleanup(func() { pauseSaves = prevPause; backupPolicyNow.Store(prevPolicy) })
	backupPolicyNow.Store(nil) // no spool: the walk runs for the whole upload
	pauseSaves = func(_ context.Context, _ *redis.Client, _ *DockerManager, uuid, _ string) *worldSaveGuard {
		return &worldSaveGuard{uuid: uuid} // the server is stopped
	}

	const uuid = "srv-f3-0001"
	for _, tc := range []struct {
		name     string
		startSub string
		want     []string
	}{
		{"the archived server starts", "", []string{"save-off", "save-all flush", "save-on"}},
		{"its active sub-server starts", "world", []string{"save-off", "save-all flush", "save-on"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core := newFakeCore(t, 1024)
			core.bucket.fail = func(part int32, _ string) int {
				if part == 1 {
					startingDuringBackup(uuid, tc.startSub)
				}
				return 0
			}
			if report := runBackupWithFakeCore(t, core, nil); report["status"] != "success" {
				t.Fatalf("report = %v", report)
			}
			got, _ := core.rdb.LRange(context.Background(), "dylaris:server:"+uuid+":input", 0, -1).Result()
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("console got %q, want %q", got, tc.want)
			}
			// After the run a start is an ordinary start again.
			startingDuringBackup(uuid, tc.startSub)
			if n, _ := core.rdb.LLen(context.Background(), "dylaris:server:"+uuid+":input").Result(); n != int64(len(tc.want)) {
				t.Fatalf("a start after the run still sent commands (%d queued)", n)
			}
		})
	}
}

// The guard pieces on their own: another sub-server starting is left alone,
// a second start sends nothing more, and a start after the walk sends nothing.
func TestPauseOnStart(t *testing.T) {
	var sent []string
	g := &worldSaveGuard{uuid: "srv-cold"}
	pacer := newBackupPacer(0)
	stop := g.pauseOnStart(func(c string) error { sent = append(sent, c); return nil }, "survival", pacer)

	startingDuringBackup("srv-cold", "creative") // another sub-server: its files are not archived
	if len(sent) != 0 || pacer.pausedSince.Load() != 0 {
		t.Fatalf("a start of another sub-server sent %v", sent)
	}
	startingDuringBackup("srv-cold", "survival")
	startingDuringBackup("srv-cold", "survival")
	if want := []string{"save-off", "save-all flush"}; !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
	if pacer.pausedSince.Load() == 0 {
		t.Fatal("the pacer does not know saving is paused, so its ceiling never lifts the pace")
	}
	stop()
	stop()
	g.resume()
	startingDuringBackup("srv-cold", "survival")
	if want := []string{"save-off", "save-all flush", "save-on"}; !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
}
