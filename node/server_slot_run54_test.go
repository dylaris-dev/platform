package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// R54-01: a storage key Redis did not answer resolved to "" like an
// unresolvable path, so a setup passed awaitServerSlot at once, beat the
// create it waited for and the create then skipped the quota and placement.
// An unread key now keeps it waiting until the create made the directory.
func TestAwaitServerSlotWaitsWhileTheStorageKeyIsUnreadable(t *testing.T) {
	const uuid = "a54a54a5-2222-3333-4444-555555555555"
	prevWait := serverSlotWait
	serverSlotWait = 400 * time.Millisecond
	t.Cleanup(func() { serverSlotWait = prevWait })
	for _, tc := range []struct {
		name    string
		created bool // Redis answers again and the create makes the directory
		want    bool
	}{
		{"redis stays down", false, false},
		{"redis back, create ran", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			root := t.TempDir()
			storage := NewStorageManager(root, rdb)
			mr.SetError("LOADING Redis is loading the dataset in memory")
			if tc.created {
				go func() {
					time.Sleep(100 * time.Millisecond)
					mr.SetError("")
					os.MkdirAll(filepath.Join(root, uuid), 0o755)
					os.WriteFile(filepath.Join(root, uuid, ".node_config.json"), []byte("{}"), 0o644)
				}()
			}
			if got := awaitServerSlot(storage, uuid); got != tc.want {
				t.Errorf("awaitServerSlot = %v, want %v", got, tc.want)
			}
		})
	}
}
