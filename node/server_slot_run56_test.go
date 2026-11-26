package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// R56-01, R56-03: a setup waiting in awaitServerSlot held a storage writer, so
// the create's move of a slot a write made saw a second writer and was
// refused. A queued setup holds none until its slot is there.
func TestAWaitingSetupDoesNotBlockTheCreatesSlotMove(t *testing.T) {
	const uuid = "a56a56a5-1111-2222-3333-444444444444"
	mr := miniredis.RunT(t)
	roots := []string{t.TempDir(), t.TempDir()}
	sm := NewStorageManager(strings.Join(roots, ","), redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	paths := sm.Paths()
	src := filepath.Join(paths[0], uuid)
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".upload.zip"), []byte("zip-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The setup's hold before its switch, taken while it waits for the slot.
	defer holdCommandWriter(sm, NodeCommand{Action: "setup", Config: ServerConfig{UUID: uuid}})()
	defer holdCommandWriter(sm, NodeCommand{Action: "create", Config: ServerConfig{UUID: uuid}})()
	got, err := sm.placeEarlySlot(uuid, paths[0], paths[1], 1)
	if err != nil {
		t.Fatalf("placeEarlySlot: %v", err)
	}
	if got != paths[1] {
		t.Errorf("placeEarlySlot = %q, want the requested %q", got, paths[1])
	}

	// The setup takes its hold once the slot is there, before it resolves the
	// path it installs into.
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := between(t, strings.ReplaceAll(string(b), "\r\n", "\n"), "\tcase \"setup\":", "\tcase \"reinstall\":")
	wait := strings.Index(body, "awaitServerSlot(storage, cmd.Config.UUID)")
	hold := strings.Index(body, "storage.holdWriter(cmd.Config.UUID)")
	resolve := strings.Index(body, "serverPath := storage.GetServerDir(cmd.Config.UUID)")
	if wait < 0 || hold < wait || resolve < hold {
		t.Errorf("setup's writer hold must sit after awaitServerSlot and before its path is resolved (wait %d, hold %d, resolve %d)", wait, hold, resolve)
	}
}

// R56-04: a setup sent while the create still pulls its image is not refused
// once serverSlotWait is up; it waits for the create it follows.
// R56-02, R56-05: a slot with none of the node's files and no create running
// (a create on an older node that could not make the container saved none)
// is let through after the wait, as before.
func TestAwaitServerSlotFollowsARunningCreate(t *testing.T) {
	const uuid = "a56a56a5-5555-6666-7777-888888888888"
	prevWait := serverSlotWait
	serverSlotWait = 200 * time.Millisecond
	t.Cleanup(func() { serverSlotWait = prevWait })
	for _, tc := range []struct {
		name     string
		makeDir  bool
		creating bool
		want     bool
	}{
		{"bare slot an older create left", true, false, true},
		{"no slot and no create", false, false, false},
		{"create still pulling", false, true, true},
		{"create still pulling over a write-made slot", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			root := t.TempDir()
			storage := NewStorageManager(root, redis.NewClient(&redis.Options{Addr: mr.Addr()}))
			dir := filepath.Join(root, uuid)
			if tc.makeDir {
				if err := os.MkdirAll(filepath.Join(dir, "survival"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.creating {
				if !commandsInFlight.enter(createKey(uuid)) {
					t.Fatal("create already marked running")
				}
				t.Cleanup(func() { commandsInFlight.leave(createKey(uuid)) })
			}
			done := make(chan bool, 1)
			go func() { done <- awaitServerSlot(storage, uuid) }()
			if tc.creating {
				select {
				case got := <-done:
					t.Fatalf("awaitServerSlot = %v while the create still runs, want it to wait", got)
				case <-time.After(3 * serverSlotWait):
				}
				// The create ends: its slot and saved config are there.
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, ".node_config.json"), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
				commandsInFlight.leave(createKey(uuid))
			}
			select {
			case got := <-done:
				if got != tc.want {
					t.Errorf("awaitServerSlot = %v, want %v", got, tc.want)
				}
			case <-time.After(10 * serverSlotWait):
				t.Fatal("awaitServerSlot did not return")
			}
		})
	}
}
