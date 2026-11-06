package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	pb "dylaris-proto/node"
)

// Nothing lowered the gauge after a delete, so on a node without project
// quotas an upload right after freeing space was refused for up to the five
// minutes until the next measurement. Copies already remeasure; delete did not.
func TestADeleteRemeasuresTheServer(t *testing.T) {
	_, rdb := gaugeRedis(t)
	prevSM, prevQ := globalStorageMgr, globalQuotaSet
	t.Cleanup(func() { globalStorageMgr, globalQuotaSet = prevSM, prevQ })
	globalStorageMgr, globalQuotaSet = NewStorageManager(t.TempDir(), rdb), nil
	const uuid = "92929292-9292-9292-9292-929292929297"
	serverDir := globalStorageMgr.GetServerDir(uuid)
	if err := os.MkdirAll(filepath.Join(serverDir, "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "world", "region.mca"), make([]byte, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "keep.txt"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	recordDiskLimit(context.Background(), rdb, uuid, 1)
	rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":900000,"limit":1048576}`, time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})

	h := &StreamHandler{storageMgr: globalStorageMgr}
	if res := h.handleDelete("r", uuid, &pb.DeleteFileReq{Path: "world"}); res.GetError() != nil {
		t.Fatalf("delete: %v", res.GetError())
	}
	want := dirSize(serverDir)
	deadline := time.Now().Add(3 * time.Second)
	for gaugeTotal(t, rdb, uuid) != want {
		if time.Now().After(deadline) {
			t.Fatalf("gauge after a delete = %d, want the measured %d", gaugeTotal(t, rdb, uuid), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The walk has to be over before the cleanup swaps the storage manager back.
	for v, _ := remeasureRuns.Load(uuid); v.(*atomic.Int64).Load() != 0; {
		time.Sleep(10 * time.Millisecond)
	}
}
