package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-pkg/queue"
)

// V04/V05: a setup that arrives while the create of its server still runs
// waits for it; a setup of another server does not.
func TestSetupWaitsForTheCreateOfItsServer(t *testing.T) {
	const busy = "c4c4c4c4-2222-3333-4444-555555555555"
	for _, tc := range []struct {
		name  string
		uuid  string
		waits bool
	}{
		{"same server", busy, true},
		{"other server", "d5d5d5d5-2222-3333-4444-555555555555", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			for _, action := range []string{"setup", "reinstall"} {
				mr := miniredis.RunT(t)
				rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
				storage := NewStorageManager(t.TempDir(), rdb)
				// The key names a path that is not mounted, so the command
				// ends at its storage guard and never touches Docker.
				rdb.Set(context.Background(), "node:"+nodeID+":server:"+tc.uuid+":storage", "/not/mounted", 0)

				release := lockServerInstall(busy) // the create of busy is running
				done := make(chan struct{})
				go func() {
					defer close(done)
					cmd := NodeCommand{Action: action}
					cmd.Config.UUID = tc.uuid
					processCommand(context.Background(), cmd, "", rdb, nil, "node-a", nil, storage)
				}()
				select {
				case <-done:
					if tc.waits {
						t.Errorf("%s ran while the create of its server held the lock", action)
					}
				case <-time.After(300 * time.Millisecond):
					if !tc.waits {
						t.Errorf("%s of another server waited for this create", action)
					}
				}
				release()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatalf("%s never ran after the create finished", action)
				}
			}
		})
	}
}

// V06: a setup or reinstall whose storage path is not mounted used to install
// into the agent's working directory and report success.
func TestSetupAndReinstallRefuseAnUnmountedStoragePath(t *testing.T) {
	for _, action := range []string{"setup", "reinstall"} {
		t.Run(action, func(t *testing.T) {
			const uuid = "e6e6e6e6-2222-3333-4444-555555555555"
			t.Chdir(t.TempDir())
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			storage := NewStorageManager(t.TempDir(), rdb)
			rdb.Set(context.Background(), "node:"+nodeID+":server:"+uuid+":storage", "/not/mounted", 0)
			sub := rdb.Subscribe(context.Background(), queue.SetupResultsChannel(nodeID))
			defer sub.Close()
			if _, err := sub.Receive(context.Background()); err != nil {
				t.Fatal(err)
			}

			cmd := NodeCommand{Action: action}
			cmd.Config.UUID = uuid
			cmd.Installer.Type = "paper"
			processCommand(context.Background(), cmd, "", rdb, nil, "node-a", nil, storage)

			if _, err := os.Stat(filepath.Join(".", "server")); !os.IsNotExist(err) {
				t.Errorf("%s wrote into the working directory: %v", action, err)
			}
			select {
			case msg := <-sub.Channel():
				if msg.Payload == "" {
					t.Error("empty setup report")
				}
			case <-time.After(2 * time.Second):
				t.Errorf("%s reported no failure for an unmounted storage path", action)
			}
		})
	}
}

// V04: a create that comes again after the server was set up (redelivered,
// or late behind its setup) must not replace the container or the saved start
// command with an empty slot.
func TestACreateLeavesASetUpServerAlone(t *testing.T) {
	const uuid = sharedDaemonUUID
	t.Cleanup(func() { forgetTombstoneInMemory(uuid) })
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	dm, d := newSharedDaemon(t, "node-a", true, nil)
	root := t.TempDir()
	storage := NewStorageManager(root, rdb)
	dm.storageMgr = storage

	dir := filepath.Join(root, uuid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	saved := []byte(`{"uuid":"` + uuid + `","activeSubServer":"survival","command":"java -jar server.jar"}`)
	os.WriteFile(filepath.Join(dir, ".node_config.json"), saved, 0o600)
	os.WriteFile(filepath.Join(dir, ".active_server"), []byte("survival"), 0o644)

	cmd := NodeCommand{Action: "create"}
	cmd.Config.UUID = uuid
	cmd.Config.Docker.Image = "img:1"
	processCommand(context.Background(), cmd, "", rdb, dm, "node-a", nil, storage)

	if ch := d.changed(); len(ch) != 0 {
		t.Errorf("create touched the container of a set-up server: %v", ch)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ".node_config.json")); string(got) != string(saved) {
		t.Errorf("create overwrote the saved config: %s", got)
	}
}
