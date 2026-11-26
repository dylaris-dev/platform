package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// R55-01: a template push or an upload ahead of the create made the server's
// directory, so setup passed awaitServerSlot at once and could beat the create
// to the install lock. Only a file the node itself writes there counts now;
// a bare directory only once the wait is up (R56-02).
func TestAwaitServerSlotWaitsForTheCreateNotTheDirectory(t *testing.T) {
	const uuid = "a55a55a5-1111-2222-3333-444444444444"
	prevWait := serverSlotWait
	serverSlotWait = 300 * time.Millisecond
	t.Cleanup(func() { serverSlotWait = prevWait })
	for _, tc := range []struct {
		name  string
		files []string
		want  bool // let through at once, not after the wait
	}{
		{"only an uploaded zip", []string{".upload.zip"}, false},
		{"empty directory", nil, false},
		{"create saved its config", []string{".upload.zip", ".node_config.json"}, true},
		{"already set up", []string{".active_server"}, true},
		{"moved in", []string{storageLiveMarker}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			root := t.TempDir()
			storage := NewStorageManager(root, redis.NewClient(&redis.Options{Addr: mr.Addr()}))
			dir := filepath.Join(root, uuid)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			if got := awaitServerSlot(storage, uuid); !got {
				t.Errorf("awaitServerSlot = false, want true")
			}
			if waited := time.Since(start) >= serverSlotWait; waited != !tc.want {
				t.Errorf("waited out serverSlotWait = %v, want %v", waited, !tc.want)
			}
		})
	}
}

// R55-01: a create over a slot a write made kept the first storage path and
// ignored the path it was asked for. It moves that slot now, unless another
// writer is still in it.
func TestPlaceEarlySlotMovesAWriteMadeSlot(t *testing.T) {
	const uuid = "a55a55a5-5555-6666-7777-888888888888"
	for _, tc := range []struct {
		name         string
		preferred    int // index into the paths, -1 for none
		otherWriter  bool
		wantPath     int
		wantUploaded int // where the uploaded zip ends up
	}{
		{"asked for the second path", 1, false, 1, 1},
		{"asked for the path it is on", 0, false, 0, 0},
		{"an upload still writing", 1, true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			roots := []string{t.TempDir(), t.TempDir()}
			sm := NewStorageManager(strings.Join(roots, ","), rdb)
			paths := sm.Paths()
			src := filepath.Join(paths[0], uuid)
			if err := os.MkdirAll(src, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, ".upload.zip"), []byte("zip-bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
			release := sm.holdWriter(uuid) // the create's own hold
			defer release()
			if tc.otherWriter {
				defer sm.holdWriter(uuid)()
			}
			preferred := ""
			if tc.preferred >= 0 {
				preferred = paths[tc.preferred]
			}
			got, err := sm.placeEarlySlot(uuid, paths[0], preferred, 1)
			if err != nil {
				t.Fatalf("placeEarlySlot: %v", err)
			}
			if got != paths[tc.wantPath] {
				t.Errorf("placeEarlySlot = %q, want %q", got, paths[tc.wantPath])
			}
			if b, err := os.ReadFile(filepath.Join(paths[tc.wantUploaded], uuid, ".upload.zip")); err != nil || string(b) != "zip-bytes" {
				t.Errorf("uploaded zip at %s: %q, %v", paths[tc.wantUploaded], b, err)
			}
			if holding := sm.PathsHolding(uuid); len(holding) != 1 || holding[0] != paths[tc.wantPath] {
				t.Errorf("copies on %v, want one on %s", holding, paths[tc.wantPath])
			}
			if key, _ := mr.Get(fmt.Sprintf("node:%s:server:%s:storage", nodeID, uuid)); tc.wantPath != 0 && key != paths[tc.wantPath] {
				t.Errorf("storage key %q, want %q", key, paths[tc.wantPath])
			}
		})
	}
}

// R55-01: a create that finds the server already set up still puts on the
// disk limit it carries when none is recorded, in the cache and the saved
// config, and saves its config even when the container could not be made, so
// a waiting setup is let through.
func TestCreateOverASetUpServerAppliesAMissingLimit(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	body := between(t, s, "\tcase \"create\":", "\tcase \"setup\":")
	early := sliceAfter(t, body, "readServerRootFile(dir, \".active_server\"); err == nil {", "\t\t\treturn\n\t\t}")
	for _, want := range []string{"knownDiskLimitMB(ctx, rdb, quota, cmd.Config.UUID) == 0", "applyDiskLimit(ctx, rdb, quota, cmd.Config.UUID, limitMB)", "recordSavedDiskLimit(dir, limitMB)"} {
		if !strings.Contains(early, want) {
			t.Errorf("create's set-up early return lacks %s", want)
		}
	}
	if !strings.Contains(body, "storage.placeEarlySlot(cmd.Config.UUID, storagePath, cmd.Config.StoragePath, 1)") {
		t.Error("create keeps a slot a write made on the first path, whatever path it was asked for")
	}
	failed := sliceAfter(t, body, "Failed to create server pod", "} else {")
	if !strings.Contains(failed, "saveNodeConfigWithDiskLimit(serverPath, cmd.Config)") {
		t.Error("a create whose container failed saves no config, so its setup waits out serverSlotWait")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".node_config.json"), []byte(`{"uuid":"x","activeSubServer":"survival"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSavedDiskLimit(dir, 4096)
	data, err := readServerRootFile(dir, ".node_config.json")
	if err != nil {
		t.Fatal(err)
	}
	var saved ServerConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Docker.DiskLimit != 4096 || saved.ActiveSubServer != "survival" {
		t.Errorf("saved config = limit %d, sub-server %q; want 4096, survival", saved.Docker.DiskLimit, saved.ActiveSubServer)
	}
}

// sliceAfter is s from the first from up to the first to after it.
func sliceAfter(t *testing.T, s, from, to string) string {
	t.Helper()
	i := strings.Index(s, from)
	if i < 0 {
		t.Fatalf("cannot find %q", from)
	}
	j := strings.Index(s[i:], to)
	if j < 0 {
		t.Fatalf("cannot find %q after %q", to, from)
	}
	return s[i : i+j]
}
