package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// R62-01: a re-tag check that got the node's only walk slot while its server
// was being moved must give the slot up, not hold it for the whole copy.
func TestARetagCheckDoesNotHoldTheSlotThroughAStorageMove(t *testing.T) {
	prevMgr := globalStorageMgr
	t.Cleanup(func() { globalStorageMgr = prevMgr })
	sm := NewStorageManager(t.TempDir(), nil)
	globalStorageMgr = sm
	const uuid = "c3d4e5f6-0000-4000-8000-000000000062"
	if err := os.MkdirAll(filepath.Join(sm.Paths()[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	rdb := newMiniRedis(t)
	if err := rdb.Set(context.Background(), diskCountedKey(uuid), "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	release, err := sm.fenceMove(uuid)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	t.Cleanup(func() {
		retagChecks.Lock()
		delete(retagChecks.next, uuid)
		retagChecks.Unlock()
	})

	retagIfMovedSoon(rdb, &QuotaSet{storage: sm}, uuid)

	deadline := time.Now().Add(2 * time.Second)
	for {
		retagChecks.Lock()
		next := retagChecks.next[uuid]
		retagChecks.Unlock()
		if next.Before(time.Now().Add(time.Hour)) {
			return // the check ended and released its claim
		}
		if time.Now().After(deadline) {
			t.Fatal("the re-tag check still holds its slot while the server's move is fenced")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
