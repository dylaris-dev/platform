package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// storageKeyGetFails fails the next n GETs of one key (n < 0: every one),
// as a Valkey that hits the lookup deadline; every other command answers.
type storageKeyGetFails struct {
	key string
	n   int
}

func (*storageKeyGetFails) DialHook(next redis.DialHook) redis.DialHook { return next }
func (*storageKeyGetFails) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *storageKeyGetFails) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" && len(cmd.Args()) > 1 && cmd.Args()[1] == h.key && h.n != 0 {
			h.n--
			err := errors.New("i/o timeout")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

// R36-06 / R36-09: a create whose storage key read failed once was dropped
// (acked, never redelivered) although nothing was on disk to protect, and the
// later setup ran the server with no slot, quota or disk limit. A transient
// failure is now read again; one that does not pass leaves the command pending
// for redelivery and changes nothing.
func TestACreateIsNeverDroppedOnAnUnreadableStorageKey(t *testing.T) {
	prev := createKeyReadDelay
	createKeyReadDelay = 0
	t.Cleanup(func() { createKeyReadDelay = prev })
	const uuid = "f8f8f8f8-2222-3333-4444-555555555555"
	for _, tc := range []struct {
		name        string
		fails       int
		wantSlot    bool
		wantRetried bool
	}{
		{"one failed read", 1, true, false},
		{"key never readable", -1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Cleanup(func() { forgetTombstoneInMemory(uuid) })
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			root := t.TempDir()
			storage := NewStorageManager(root, rdb)
			key := "node:" + nodeID + ":server:" + uuid + ":storage"
			hook := &storageKeyGetFails{key: key, n: tc.fails}
			rdb.AddHook(hook)

			cmd := NodeCommand{Action: "create"}
			cmd.Config.UUID = uuid
			ctx, redeliver := withRedelivery(context.Background())
			func() {
				defer func() { _ = recover() }() // nil DockerManager: a create that goes on panics there
				processCommand(ctx, cmd, "", rdb, nil, "node-a", nil, storage)
			}()
			hook.n = 0

			_, err := os.Stat(filepath.Join(root, uuid))
			if slot := err == nil; slot != tc.wantSlot {
				t.Errorf("slot made = %v, want %v", slot, tc.wantSlot)
			}
			if got := *redeliver != nil; got != tc.wantRetried {
				t.Errorf("left for redelivery = %v (%v), want %v", got, *redeliver, tc.wantRetried)
			}
			if !tc.wantSlot && mr.Exists(key) {
				t.Errorf("a create left for redelivery wrote the storage key")
			}
		})
	}
}

// R36-07 / R36-10: a create redelivered before setup ran SelectStoragePath
// again, which could pick a second path and re-key the server there, leaving
// two unmarked copies that resolve nowhere after a key loss. The slot an
// earlier delivery made is reused.
func TestARedeliveredCreateReusesItsSlot(t *testing.T) {
	const uuid = "f9f9f9f9-2222-3333-4444-555555555555"
	t.Chdir(t.TempDir())
	t.Cleanup(func() { forgetTombstoneInMemory(uuid) })
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	storage := NewStorageManager(t.TempDir()+","+t.TempDir(), rdb)
	paths := storage.Paths()
	if _, err := storage.assignPath(uuid, paths[0]); err != nil {
		t.Fatal(err)
	}

	cmd := NodeCommand{Action: "create"}
	cmd.Config.UUID = uuid
	// What auto placement picks the second time is free space; a named path
	// makes the second pick certain.
	cmd.Config.StoragePath = paths[1]
	func() {
		defer func() { _ = recover() }()
		processCommand(context.Background(), cmd, "", rdb, nil, "node-a", nil, storage)
	}()

	if got := rdb.Get(context.Background(), "node:"+nodeID+":server:"+uuid+":storage").Val(); got != paths[0] {
		t.Errorf("storage key = %q, want the existing slot %q", got, paths[0])
	}
	if got := storage.PathsHolding(uuid); len(got) != 1 {
		t.Errorf("PathsHolding = %v, want only the first slot", got)
	}
}

// R36-08: an unsure move whose key re-reads all failed left both copies tied
// for good; once Redis answered again nothing re-marked them, so a later key
// loss made the server unresolvable. The next lookup that reads the key
// marks the copy it does not name moved away. Nothing is deleted.
func TestAnUnsureMoveIsSettledOnceTheKeyIsReadable(t *testing.T) {
	prevDelay := unsureKeyRereadDelay
	unsureKeyRereadDelay = 0
	t.Cleanup(func() { unsureKeyRereadDelay = prevDelay })
	for _, tc := range []struct {
		name     string
		applySet bool
		live     int
	}{
		{"set applied", true, 1},
		{"set refused", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			sm := NewStorageManager(t.TempDir()+","+t.TempDir(), rdb)
			paths := sm.Paths()
			src := filepath.Join(paths[0], run18UUID)
			if err := os.MkdirAll(src, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, storageLiveMarker), []byte("100"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := sm.setRedisPath(run18UUID, paths[0]); err != nil {
				t.Fatal(err)
			}
			hook := &flakyStorageKey{applySet: tc.applySet, getFails: -1}
			rdb.AddHook(hook)
			prev := migrateCopyTree
			t.Cleanup(func() { migrateCopyTree = prev })
			migrateCopyTree = func(s, d string) error {
				hook.on = true // after resolveMigration's read
				return prev(s, d)
			}
			if err := sm.MigrateServerPath(run18UUID, paths[1]); !errors.Is(err, errStorageKeyUnsure) {
				t.Fatalf("MigrateServerPath = %v, want errStorageKeyUnsure", err)
			}
			hook.on = false

			if got := sm.GetServerPath(run18UUID); got != paths[tc.live] {
				t.Fatalf("GetServerPath = %q, want %q", got, paths[tc.live])
			}
			mr.FlushAll()
			if got := sm.GetServerPath(run18UUID); got != paths[tc.live] {
				t.Fatalf("after the key loss GetServerPath = %q, want %q", got, paths[tc.live])
			}
			if got := sm.PathsHolding(run18UUID); len(got) != 2 {
				t.Errorf("PathsHolding = %v, want both copies kept", got)
			}
		})
	}
}
