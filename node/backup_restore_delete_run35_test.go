package main

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-pkg/queue"
)

// R35-06: the old copy was credited back only once its RemoveAll returned, so
// a write budget opened during the delete measured the space already freed and
// was then granted it a second time. What such a budget charges itself for
// others, landed plus written-in-flight, must not drop after it opened.
func TestARestoreCreditsTheOldCopyBeforeDeletingIt(t *testing.T) {
	root, cmd := restoreArchive(t, []tar.Header{
		{Name: "world/r.0.0.mca", Mode: 0o644, Typeflag: tar.TypeReg, Size: 1 << 20},
	}, map[string]string{"world/r.0.0.mca": strings.Repeat("r", 1<<20)})
	if cmd.SubServer != "" {
		t.Fatalf("fixture restores sub-server %q, want a whole-server restore", cmd.SubServer)
	}
	if err := os.WriteFile(filepath.Join(root, cmd.ServerUUID, "old.mca"), []byte(strings.Repeat("x", 1<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	held := func() int64 { w := writesOn(""); return w.onDisk.Load() + w.landed.Load() }
	prev := removeOldCopy
	t.Cleanup(func() { removeOldCopy = prev })
	var midDelete int64
	deleted := false
	removeOldCopy = func(p string) error {
		err := os.RemoveAll(p)
		deleted = true
		midDelete = held()
		return err
	}
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "success" {
		t.Fatalf("report = %v", report)
	}
	if !deleted {
		t.Fatal("the restore never deleted the old copy")
	}
	if got := held(); got < midDelete {
		t.Errorf("held bytes dropped by %d after the delete of the old copy, want 0: a budget opened during it was credited the old copy a second time", midDelete-got)
	}
}
