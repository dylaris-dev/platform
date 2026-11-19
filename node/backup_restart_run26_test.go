package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/redis/go-redis/v9"
)

// R26-03/R26-04: save-off is per JVM, so a server that was RUNNING when its
// backup began and is restarted (power restart, reconciler crash restart)
// mid-walk boots a JVM that saves while tar reads the world. Every start
// during the walk must pause saving again; the run still sends save-on once.
func TestARestartDuringABackupOfARunningServerPausesSavingAgain(t *testing.T) {
	prevPause, prevPolicy := pauseSaves, backupPolicyNow.Load()
	t.Cleanup(func() { pauseSaves = prevPause; backupPolicyNow.Store(prevPolicy) })
	backupPolicyNow.Store(nil) // no spool: the walk runs for the whole upload
	const uuid = "srv-f3-0001" // the run runBackupWithFakeCore starts
	inputKey := "dylaris:server:" + uuid + ":input"
	pauseSaves = func(_ context.Context, rdb *redis.Client, _ *DockerManager, id, _ string) *worldSaveGuard {
		// The server is running: the first JVM gets save-off at the start.
		send := func(c string) error { return rdb.RPush(context.Background(), inputKey, c).Err() }
		send("save-off")
		send("save-all flush")
		return &worldSaveGuard{uuid: id, send: send}
	}

	for _, tc := range []struct {
		name   string
		starts int
		want   []string
	}{
		{"one restart", 1, []string{"save-off", "save-all flush", "save-off", "save-all flush", "save-on"}},
		{"two restarts", 2, []string{"save-off", "save-all flush", "save-off", "save-all flush", "save-off", "save-all flush", "save-on"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core := newFakeCore(t, 1024)
			core.bucket.fail = func(part int32, _ string) int {
				if part == 1 {
					for i := 0; i < tc.starts; i++ {
						startingDuringBackup(uuid, "")
					}
				}
				return 0
			}
			if report := runBackupWithFakeCore(t, core, nil); report["status"] != "success" {
				t.Fatalf("report = %v", report)
			}
			got, _ := core.rdb.LRange(context.Background(), inputKey, 0, -1).Result()
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("console got %q, want %q", got, tc.want)
			}
		})
	}
}
