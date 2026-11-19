package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// R30-05: a JVM that crashes mid-walk is restarted by the log-shipper inside
// the same container, and no node start path runs for it, so the new JVM
// booted with saving on and wrote region files while tar read them. The
// shipper's restart line on a console the run watches pauses saving again.
func TestACrashRestartInsideTheContainerDuringABackupPausesSavingAgain(t *testing.T) {
	prevPause, prevPolicy := pauseSaves, backupPolicyNow.Load()
	t.Cleanup(func() { pauseSaves = prevPause; backupPolicyNow.Store(prevPolicy) })
	backupPolicyNow.Store(nil) // no spool: the walk runs for the whole upload
	const uuid = "srv-f3-0001" // the run runBackupWithFakeCore starts
	inputKey := "dylaris:server:" + uuid + ":input"
	const crashed = "[Server crashed (exit code 137) - restarting automatically (1/5)...]"
	paused := []string{"save-off", "save-all flush"}
	resumed := []string{"save-on"}

	for _, tc := range []struct {
		name    string
		running bool   // the server ran when the backup began
		start   string // a start during the walk, of this sub-server ("-" for none)
		console string // the console the line is written to
		line    string
		want    [][]string
	}{
		{"a running server crash-restarts", true, "-", "", crashed, [][]string{paused, paused, resumed}},
		{"a server started mid-walk crash-restarts", false, "world", "world", crashed, [][]string{paused, paused, resumed}},
		{"the shipper gives up", true, "-", "", "[Server crashed 5 times within 10m0s (last exit code 1) - giving up. ...]", [][]string{paused, resumed}},
		{"a player says it", true, "-", "", "[12:00:00 INFO]: <Steve> [Server crashed (exit code 1) - restarting automatically", [][]string{paused, resumed}},
		{"another console", true, "-", "creative", crashed, [][]string{paused, resumed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pauseSaves = func(_ context.Context, rdb *redis.Client, _ *DockerManager, id, _ string) *worldSaveGuard {
				g := &worldSaveGuard{uuid: id}
				if tc.running {
					g.send = func(c string) error { return rdb.RPush(context.Background(), inputKey, c).Err() }
					g.send("save-off")
					g.send("save-all flush")
					g.subs = map[string]bool{"": true}
				}
				return g
			}
			var want []string
			for _, w := range tc.want {
				want = append(want, w...)
			}
			core := newFakeCore(t, 1024)
			core.bucket.fail = func(part int32, _ string) int {
				if part != 1 {
					return 0
				}
				ctx := context.Background()
				if tc.start != "-" {
					startingDuringBackup(uuid, tc.start)
				}
				// Every case has paused saving once by now.
				before := int64(len(paused))
				// The watcher reads from the console's newest line when it sees
				// the console, so the line goes out once it has had the chance.
				time.Sleep(300 * time.Millisecond)
				core.rdb.XAdd(ctx, &redis.XAddArgs{Stream: serverLogStreamKey(uuid, tc.console), Values: map[string]interface{}{"line": tc.line}})
				wantMid := int64(len(want) - len(resumed))
				for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
					if n, _ := core.rdb.LLen(ctx, inputKey).Result(); n >= wantMid && wantMid > before {
						break
					}
				}
				if wantMid == before {
					time.Sleep(300 * time.Millisecond)
				}
				return 0
			}
			if report := runBackupWithFakeCore(t, core, nil); report["status"] != "success" {
				t.Fatalf("report = %v", report)
			}
			got, _ := core.rdb.LRange(context.Background(), inputKey, 0, -1).Result()
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("console got %q, want %q", got, want)
			}
		})
	}
}
