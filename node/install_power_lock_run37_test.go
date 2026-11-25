package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// R37-01: setup, reinstall and a version move took no power lock, so a
// restart already in its graceful stop recreated and started the old
// container while the installer was stashing jars and writing into the tree.
// They now wait for the running power action before they touch anything.
func TestInstallsWaitForTheServersPowerAction(t *testing.T) {
	cases := []struct {
		action  string
		payload string
	}{
		{action: "setup"},
		{action: "reinstall"},
		{action: "update_server_version"},
	}
	for i, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			uuid := "37373737-0101-0101-0101-37373737373" + string(rune('0'+i))
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, uuid, "server"), 0o755); err != nil {
				t.Fatal(err)
			}
			// The create's saved config, which setup waits for (awaitServerSlot).
			if err := os.WriteFile(filepath.Join(root, uuid, ".node_config.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			payload := `{"config":{"uuid":"` + uuid + `","activeSubServer":"server"}}`
			release, ok := tryLockServerPower(uuid)
			if !ok {
				t.Fatal("power lock taken")
			}
			released := false
			defer func() {
				if !released {
					release()
				}
			}()
			rdb := newMiniRedis(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				cmd := NodeCommand{Action: tc.action}
				cmd.Config.UUID = uuid
				cmd.Config.ActiveSubServer = "server"
				processCommand(context.Background(), cmd, payload, rdb, dockerWithWorkingDir(t, ""), "node-a", nil, &StorageManager{paths: []string{root}})
			}()
			select {
			case <-done:
				t.Fatalf("%s ran while a power action held the server", tc.action)
			case <-time.After(500 * time.Millisecond):
			}
			// The reinstall sleeps after its stop, so it is still running here
			// either way; its busy hold says whether it got past the wait.
			if n, _ := rdb.Exists(context.Background(), nodeBusyKey(uuid)).Result(); n != 0 {
				t.Fatalf("%s started its work while a power action held the server", tc.action)
			}
			release()
			released = true
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatalf("%s did not run once the power action ended", tc.action)
			}
		})
	}
}
