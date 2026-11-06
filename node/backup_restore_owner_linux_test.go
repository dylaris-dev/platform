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

// A restore onto a missing server root keeps the archive's node files, and
// they were handed to the container's uid (or linked to a file that was):
// the tenant could rewrite the config the reconciler recreates the container
// from.
func TestRestoredNodeFilesStayTheNodes(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root; asserted in CI's container and by the live test")
	}
	t.Setenv("MC_RUN_AS", "1000")
	root, cmd := restoreArchive(t, []tar.Header{
		{Name: "ops.json", Mode: 0o644, Typeflag: tar.TypeReg},
		{Name: ".node_config.json", Mode: 0o644, Typeflag: tar.TypeReg},
		{Name: ".dylaris.json", Mode: 0o644, Typeflag: tar.TypeLink, Linkname: "ops.json"},
	}, map[string]string{"ops.json": "[]", ".node_config.json": "{}"})
	if err := os.RemoveAll(filepath.Join(root, cmd.ServerUUID)); err != nil {
		t.Fatal(err)
	}
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "success" {
		t.Fatalf("report = %v", report)
	}
	fi, err := os.Lstat(filepath.Join(root, cmd.ServerUUID, ".node_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ownedBy(fi, 1000) {
		t.Error(".node_config.json was handed to the container's uid")
	}
	if _, err := os.Lstat(filepath.Join(root, cmd.ServerUUID, ".dylaris.json")); !os.IsNotExist(err) {
		t.Errorf(".dylaris.json was linked to a tenant file (err=%v)", err)
	}
}
