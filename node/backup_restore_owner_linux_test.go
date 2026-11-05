package main

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"testing"

	"dylaris-pkg/queue"
)

// A restored hard-link pair was created as root, and the start-time ownership
// walk skips every file with a second name, so both names stayed root's and
// the server could not write them.
func TestRestoredLinkedFileBelongsToTheContainer(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root; asserted in CI's container and by the live test")
	}
	t.Setenv("MC_RUN_AS", "1000")
	root, cmd := restoreArchive(t, []tar.Header{
		{Name: "ops.json", Mode: 0o644, Typeflag: tar.TypeReg},
		{Name: "lobby/ops.json", Mode: 0o644, Typeflag: tar.TypeLink, Linkname: "ops.json"},
	}, map[string]string{"ops.json": "[]"})
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "success" {
		t.Fatalf("report = %v", report)
	}
	for _, n := range []string{"ops.json", "lobby/ops.json"} {
		fi, err := os.Lstat(filepath.Join(root, cmd.ServerUUID, n))
		if err != nil {
			t.Fatal(err)
		}
		if !ownedBy(fi, 1000) {
			t.Errorf("%s is root's after the restore", n)
		}
	}
}
