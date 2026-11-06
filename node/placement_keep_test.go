package main

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A manual placement policy must not flip to auto because one round could not
// read it: a Redis blip or restart then placed new servers on whichever disk had
// the most free space, the system disk included. A missing key (redis.Nil) is
// "no policy" and still means auto.
func TestStoragePlacementSurvivesAFailedRead(t *testing.T) {
	origRouting, origFile, origPort, origCPort, origIO, origPids := getModes()
	prevMgr, prevID := globalStorageMgr, nodeID
	t.Cleanup(func() {
		setModes(origRouting, origFile, origPort, origCPort, origIO, origPids)
		globalStorageMgr, nodeID = prevMgr, prevID
	})
	nodeID = "node-a"
	globalStorageMgr = NewStorageManager(t.TempDir(), nil)
	ctx := context.Background()

	for _, tt := range []struct {
		name     string
		down     bool
		wantMode string
	}{
		{"redis unreachable keeps the policy", true, placementManual},
		{"key gone means auto", false, placementAuto},
	} {
		globalStorageMgr.SetPlacement(placementManual, []string{"/mnt/fast"})
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
		if tt.down {
			mr.Close()
		}
		loadModesFromRedis(ctx, rdb)
		globalStorageMgr.mu.Lock()
		got := globalStorageMgr.placementMode
		globalStorageMgr.mu.Unlock()
		if got != tt.wantMode {
			t.Errorf("%s: placement mode %q, want %q", tt.name, got, tt.wantMode)
		}
		_ = rdb.Close()
	}
}
