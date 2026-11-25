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

	"dylaris-pkg/queue"
)

// X03: a setup that reached the install lock before the create of its server
// installed into paths[0], ignoring the storage path the create was asked for,
// and the create then saw .active_server and skipped placement and the quota.
// The setup now waits for the create to have made the server's directory.
func TestSetupWaitsForTheCreateToMakeTheServerDir(t *testing.T) {
	const uuid = "a3a3a3a3-2222-3333-4444-555555555555"
	prevWait := serverSlotWait
	serverSlotWait = 300 * time.Millisecond
	t.Cleanup(func() { serverSlotWait = prevWait })
	for _, tc := range []struct {
		name    string
		created bool // the create made the directory during the wait
		want    string
	}{
		{"create never ran", false, "not been created"},
		{"create ran meanwhile", true, "not-an-installer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			root := t.TempDir()
			storage := NewStorageManager(root, rdb)
			sub := rdb.Subscribe(context.Background(), queue.SetupResultsChannel(nodeID))
			defer sub.Close()
			if _, err := sub.Receive(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tc.created {
				go func() {
					time.Sleep(100 * time.Millisecond)
					os.MkdirAll(filepath.Join(root, uuid), 0o755)
					os.WriteFile(filepath.Join(root, uuid, ".node_config.json"), []byte("{}"), 0o644)
				}()
			}

			cmd := NodeCommand{Action: "setup", Installer: InstallerConfig{Type: "not-an-installer"}}
			cmd.Config.UUID = uuid
			cmd.Config.ActiveSubServer = "main"
			processCommand(context.Background(), cmd, "", rdb, nil, "node-a", nil, storage)

			if !tc.created {
				if _, err := os.Stat(filepath.Join(root, uuid)); !os.IsNotExist(err) {
					t.Errorf("setup made the server directory itself: %v", err)
				}
			}
			select {
			case msg := <-sub.Channel():
				if !strings.Contains(msg.Payload, tc.want) {
					t.Errorf("report %s, want it to mention %q", msg.Payload, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Error("setup reported nothing")
			}
		})
	}
}

// X04: a version move refused beside a backup wrote no status, so Core landed
// the "installing" row as "stopped" for a server that went on running. The
// refusal reports what Docker shows, unless another command's busy hold (a
// restore) is reporting its own.
func TestVersionMoveRefusedBesideABackupReportsTheRealStatus(t *testing.T) {
	const uuid = sharedDaemonUUID
	for _, tc := range []struct {
		name     string
		hold     func() func()
		nodeBusy string
		want     string
	}{
		{"backup", func() func() {
			backupsInFlight.enter("server:" + uuid)
			return func() { backupsInFlight.leave("server:" + uuid) }
		}, "", "online"},
		{"restore", func() func() {
			restoreServersInFlight.enter(uuid)
			return func() { restoreServersInFlight.leave(uuid) }
		}, "restarting", "restarting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			dm, _ := newSharedDaemon(t, "node-a", true, nil)
			root := t.TempDir()
			storage := NewStorageManager(root, rdb)
			if err := os.MkdirAll(filepath.Join(root, uuid, "server"), 0o755); err != nil {
				t.Fatal(err)
			}
			statusKey := "dylaris:server:" + uuid + ":status"
			if tc.nodeBusy != "" {
				mr.Set(nodeBusyKey(uuid), tc.nodeBusy)
				mr.Set(statusKey, tc.nodeBusy)
			}
			defer tc.hold()()

			cmd := NodeCommand{Action: "update_server_version"}
			cmd.Installer.Type = "paper"
			runUpdateServerVersion(context.Background(), rdb, dm, storage, cmd, `{"config":{"uuid":"`+uuid+`"}}`)

			if got, _ := mr.Get(statusKey); got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
			if !mr.Exists("dylaris:server:" + uuid + ":version_update_failed") {
				t.Error("the refusal was not reported to Core")
			}
		})
	}
}

// X06: a stash a dead version move left behind outlived the setup that
// repaired the sub-server, and the next reinstall or move put the weeks-old
// jars back over the newer install. Setup recovers it before it installs.
func TestSetupRecoversAStrandedJarStash(t *testing.T) {
	const uuid = "a6a6a6a6-2222-3333-4444-555555555555"
	t.Chdir(t.TempDir())
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	root := t.TempDir()
	storage := NewStorageManager(root, rdb)
	dir := filepath.Join(root, uuid)
	stash := filepath.Join(dir, ".main.oldjars")
	if err := os.MkdirAll(filepath.Join(stash, "main", "mods"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The sub-server is there: a stash of a missing one is dropped (R24-06).
	if err := os.MkdirAll(filepath.Join(dir, "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(stash, stashCompleteMark), nil, 0o644)
	os.WriteFile(filepath.Join(stash, "main", "mods", "old.jar"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(dir, ".node_config.json"), []byte("{}"), 0o644)

	cmd := NodeCommand{Action: "setup", Installer: InstallerConfig{Type: "not-an-installer"}}
	cmd.Config.UUID = uuid
	cmd.Config.ActiveSubServer = "main"
	processCommand(context.Background(), cmd, "", rdb, nil, "node-a", nil, storage)

	if _, err := os.Stat(stash); !os.IsNotExist(err) {
		t.Errorf("the stranded stash outlived the setup: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "main", "mods", "old.jar")); err != nil || string(got) != "old" {
		t.Errorf("the stashed jar was not put back: %q, %v", got, err)
	}
}
