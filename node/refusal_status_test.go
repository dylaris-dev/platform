package main

import (
	"context"
	"os"
	"path/filepath"
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
		setUp   bool // the server has software (.active_server) on disk
		want    string
	}{
		{"backup", []string{"setup", "reinstall"}, func() func() {
			backupsInFlight.enter("server:" + uuid)
			return func() { backupsInFlight.leave("server:" + uuid) }
		}, false, true, "stopped"},
		{"version move without its hold yet", []string{"setup", "reinstall"}, func() func() {
			commandsInFlight.enter(softwareKey(uuid))
			return func() { commandsInFlight.leave(softwareKey(uuid)) }
		}, false, true, "stopped"},
		{"restore holding busy", []string{"setup", "reinstall"}, func() func() {
			restoreServersInFlight.enter(uuid)
			return func() { restoreServersInFlight.leave(uuid) }
		}, true, true, ""},
		// R25-02: a FIRST setup refused leaves the server unset-up, and
		// "stopped" made the panel offer Start on a server with no software.
		{"backup of a server never set up", []string{"setup"}, func() func() {
			backupsInFlight.enter("server:" + uuid)
			return func() { backupsInFlight.leave("server:" + uuid) }
		}, false, false, "pending_setup"},
		{"create never ran", []string{"setup"}, func() func() {
			old := serverSlotWait
			serverSlotWait = 0
			return func() { serverSlotWait = old }
		}, false, false, "pending_setup"},
	} {
		for _, action := range tc.actions {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				t.Chdir(t.TempDir())
				mr := miniredis.RunT(t)
				rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
				// Its directory is not made unless set up, so awaitServerSlot
				// finds no slot for "create never ran".
				storage := NewStorageManager(t.TempDir(), rdb)
				if tc.setUp {
					dir := storage.GetServerDir(uuid)
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, ".active_server"), []byte("server"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				ctx := context.Background()
				if tc.busy {
					release := holdBusyStatus(rdb, uuid, "restoring", busyStatusTTL)
					defer release("")
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

// R25-02: a first install that fails is put back to a server with no software,
// so it reports "pending_setup" and the panel keeps routing the owner to
// Setup. A failed re-setup of an installed server stays "stopped".
func TestAFailedSetupReportsPendingSetupUntilTheServerHasSoftware(t *testing.T) {
	const uuid = "a8a8a8a8-6666-3333-4444-555555555555"
	statusKey := "dylaris:server:" + uuid + ":status"
	for _, tc := range []struct {
		name  string
		setUp bool
		want  string
	}{
		{"first setup", false, "pending_setup"},
		{"setup over installed software", true, "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			storage := NewStorageManager(t.TempDir(), rdb)
			dir := storage.GetServerDir(uuid)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.setUp {
				if err := os.WriteFile(filepath.Join(dir, ".active_server"), []byte("server"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			cmd := NodeCommand{Action: "setup"}
			cmd.Config.UUID = uuid
			cmd.Installer.Type = "no-such-installer" // InstallServer fails, putBack runs
			processCommand(ctx, cmd, "", rdb, nil, "node-a", nil, storage)
			if got := rdb.Get(ctx, statusKey).Val(); got != tc.want {
				t.Errorf("status after the failed setup = %q, want %q", got, tc.want)
			}
		})
	}
}
