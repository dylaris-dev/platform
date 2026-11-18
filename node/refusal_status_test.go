package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// R24-01: Core wrote "installing" before it sent a setup or reinstall. A
// refusal before the command took its busy hold wrote no status at all, so a
// stopped server refused beside a backup stayed "installing" in Core for good.
// It now reports the status the server is in, unless another command's busy
// hold (a restore, a version move) is reporting one.
func TestARefusedSetupOrReinstallReportsTheServersStatus(t *testing.T) {
	const uuid = "a8a8a8a8-2222-3333-4444-555555555555"
	statusKey := "dylaris:server:" + uuid + ":status"
	for _, tc := range []struct {
		name    string
		actions []string
		hold    func() func()
		busy    bool
		want    string
	}{
		{"backup", []string{"setup", "reinstall"}, func() func() {
			backupsInFlight.enter("server:" + uuid)
			return func() { backupsInFlight.leave("server:" + uuid) }
		}, false, "stopped"},
		{"version move without its hold yet", []string{"setup", "reinstall"}, func() func() {
			commandsInFlight.enter(softwareKey(uuid))
			return func() { commandsInFlight.leave(softwareKey(uuid)) }
		}, false, "stopped"},
		{"restore holding busy", []string{"setup", "reinstall"}, func() func() {
			restoreServersInFlight.enter(uuid)
			return func() { restoreServersInFlight.leave(uuid) }
		}, true, ""},
		{"create never ran", []string{"setup"}, func() func() {
			old := serverSlotWait
			serverSlotWait = 0
			return func() { serverSlotWait = old }
		}, false, "stopped"},
	} {
		for _, action := range tc.actions {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				t.Chdir(t.TempDir())
				mr := miniredis.RunT(t)
				rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
				// Its directory is not made, so awaitServerSlot finds no slot.
				storage := NewStorageManager(t.TempDir(), rdb)
				ctx := context.Background()
				if tc.busy {
					release := holdBusyStatus(rdb, uuid, "restoring", busyStatusTTL)
					defer release()
					rdb.Del(ctx, statusKey) // drained by Core's watcher
				}
				defer tc.hold()()

				cmd := NodeCommand{Action: action}
				cmd.Config.UUID = uuid
				cmd.Installer.Type = "paper"
				done := make(chan struct{})
				go func() {
					defer close(done)
					processCommand(ctx, cmd, "", rdb, nil, "node-a", nil, storage)
				}()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatalf("%s was not refused", action)
				}
				if got := rdb.Get(ctx, statusKey).Val(); got != tc.want {
					t.Errorf("status after the refused %s = %q, want %q", action, got, tc.want)
				}
			})
		}
	}
}
