package main

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"dylaris-pkg/queue"
)

// R33-06: a whole-server restore credited the old copy it deleted only once
// the quota re-tag walk had finished, so a write budget opened during the walk
// measured the free space without the old world and was then granted it a
// second time. The settlement now lands before the re-tag starts.
func TestARestoreSettlesItsBudgetBeforeTheRetagWalk(t *testing.T) {
	root, cmd := restoreArchive(t, []tar.Header{
		{Name: "world/r.0.0.mca", Mode: 0o644, Typeflag: tar.TypeReg},
	}, map[string]string{"world/r.0.0.mca": "region bytes"})
	if cmd.SubServer != "" {
		t.Fatalf("fixture restores sub-server %q, want a whole-server restore", cmd.SubServer)
	}
	if err := os.WriteFile(filepath.Join(root, cmd.ServerUUID, "old.mca"), []byte(strings.Repeat("x", 1<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := retagAfterRestore
	t.Cleanup(func() { retagAfterRestore = prev })
	// What a write budget opened during the re-tag charges itself for others:
	// inflight plus landed. It measured the free space with the old copy
	// already gone, so that sum must not drop after it opened.
	held := func() int64 { w := writesOn(""); return w.inflight.Load() + w.landed.Load() }
	var atRetag int64
	retagged := false
	retagAfterRestore = func(context.Context, *redis.Client, string) {
		retagged = true
		atRetag = held()
	}
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "success" {
		t.Fatalf("report = %v", report)
	}
	if !retagged {
		t.Fatal("the restore never reached the re-tag")
	}
	if got := held(); got != atRetag {
		t.Errorf("held bytes moved by %d after the re-tag walk started, want 0: a budget opened during it was credited the old copy a second time", got-atRetag)
	}
}
