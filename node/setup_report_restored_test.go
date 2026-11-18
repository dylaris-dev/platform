package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Core drops a sub-server's mod rows when the setup wipes mods/, and used to
// have no way of telling a failure that left the disk as it was (a refusal,
// or one the stash rolled back) from one that wiped it. The report now says
// "restored" when the files are back, so Core can put the rows back too.
func TestSetupFailureReportSaysWhetherTheFilesAreBack(t *testing.T) {
	const uuid = "a3a3a3a3-6666-3333-4444-555555555555"
	prevWait := serverSlotWait
	serverSlotWait = 50 * time.Millisecond
	t.Cleanup(func() { serverSlotWait = prevWait })
	for _, tc := range []struct {
		name     string
		existing bool // the sub-server is installed, with a mod
		typ      string
		wipe     []string
		restored bool
	}{
		{"refused before the disk", false, "paper", []string{WipeMods}, true},
		{"rolled back by the stash", true, "paper", []string{WipeMods, "no-such-token"}, true},
		{"no stash for the installer", true, "not-an-installer", []string{WipeMods}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			root := t.TempDir()
			storage := NewStorageManager(root, rdb)
			if tc.existing {
				writeTestFile(t, filepath.Join(root, uuid, "main", "mods", "a.jar"), "mod")
			}
			sub := rdb.Subscribe(context.Background(), queue.SetupResultsChannel(nodeID))
			defer sub.Close()
			if _, err := sub.Receive(context.Background()); err != nil {
				t.Fatal(err)
			}

			cmd := NodeCommand{Action: "setup", Installer: InstallerConfig{Type: tc.typ, WipePaths: tc.wipe}}
			cmd.Config.UUID = uuid
			cmd.Config.ActiveSubServer = "main"
			processCommand(context.Background(), cmd, "", rdb, nil, "node-a", nil, storage)

			select {
			case msg := <-sub.Channel():
				var rep struct {
					Error    string `json:"error"`
					Restored bool   `json:"restored"`
				}
				if err := json.Unmarshal([]byte(msg.Payload), &rep); err != nil || rep.Error == "" {
					t.Fatalf("report %s is not a failure (%v)", msg.Payload, err)
				}
				if rep.Restored != tc.restored {
					t.Errorf("report %s: restored = %v, want %v", msg.Payload, rep.Restored, tc.restored)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("setup reported nothing")
			}
			if tc.restored && tc.existing {
				if got := readTestFile(filepath.Join(root, uuid, "main", "mods", "a.jar")); got != "mod" {
					t.Errorf("mods/a.jar = %q after a rolled-back setup", got)
				}
			}
			if _, err := os.Lstat(filepath.Join(root, uuid, ".main.oldjars")); !os.IsNotExist(err) {
				t.Errorf("the stash is still there: %v", err)
			}
		})
	}
}

// A redelivered setup refused before its recovery (here: a backup holds the
// tree) said "restored" while the old mods and jars still sat in the stash
// the dead attempt left. Core then put back mod rows for files not on disk.
func TestSetupRefusalOverAStrandedStashIsNotRestored(t *testing.T) {
	const uuid = "a3a3a3a3-6666-3333-4444-666666666666"
	for _, tc := range []struct {
		name     string
		stranded bool
		restored bool
	}{
		{"nothing stranded", false, true},
		{"stash stranded", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			root := t.TempDir()
			storage := NewStorageManager(root, rdb)
			writeTestFile(t, filepath.Join(root, uuid, "main", "mods", "b.jar"), "new mod")
			if tc.stranded {
				writeTestFile(t, filepath.Join(root, uuid, ".main.oldjars", "main", "mods", "a.jar"), "old mod")
			}
			if !backupsInFlight.enter("server:" + uuid) {
				t.Fatal("backup slot taken")
			}
			defer backupsInFlight.leave("server:" + uuid)
			sub := rdb.Subscribe(context.Background(), queue.SetupResultsChannel(nodeID))
			defer sub.Close()
			if _, err := sub.Receive(context.Background()); err != nil {
				t.Fatal(err)
			}

			cmd := NodeCommand{Action: "setup", Installer: InstallerConfig{Type: "modpack", WipePaths: []string{WipeMods}}}
			cmd.Config.UUID = uuid
			cmd.Config.ActiveSubServer = "main"
			processCommand(context.Background(), cmd, "", rdb, nil, "node-a", nil, storage)

			select {
			case msg := <-sub.Channel():
				var rep struct {
					Error    string `json:"error"`
					Restored bool   `json:"restored"`
				}
				if err := json.Unmarshal([]byte(msg.Payload), &rep); err != nil || rep.Error == "" {
					t.Fatalf("report %s is not a failure (%v)", msg.Payload, err)
				}
				if rep.Restored != tc.restored {
					t.Errorf("report %s: restored = %v, want %v", msg.Payload, rep.Restored, tc.restored)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("setup reported nothing")
			}
		})
	}
}
