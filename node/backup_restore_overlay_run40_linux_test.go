package main

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"testing"

	"dylaris-pkg/queue"
)

// The overlay seed opened the live sub-server by path, so a sub-server
// directory a root tenant replaced with a link made the node copy whatever the
// link named - another server, the node's storage path - into the tenant's tree
// (R40-03). It must be reached like the stage: through the server root, as a
// real directory.
func TestOverlaySeedDoesNotFollowASubServerLink(t *testing.T) {
	root, cmd := restoreArchive(t, []tar.Header{
		{Name: "world/r.0.0.mca", Mode: 0o644, Typeflag: tar.TypeReg},
	}, map[string]string{"world/r.0.0.mca": "restored region"})
	cmd.SubServer, cmd.Overlay = "survival", true
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, cmd.ServerUUID, "survival")
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] == "success" {
		t.Errorf("restore through a linked sub-server reported success: %v", report)
	}
	if _, err := os.Lstat(filepath.Join(target, "secret.txt")); err == nil {
		if fi, _ := os.Lstat(target); fi != nil && fi.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the linked directory's files were copied into the sub-server")
		}
	}
	if b, err := os.ReadFile(filepath.Join(outside, "secret.txt")); err != nil || string(b) != "host secret" {
		t.Fatalf("outside file = %q, %v", b, err)
	}
}

// A live file that is a second name of one of the node's own files was copied
// into the stage as a fresh file and handed to the MC uid, which gave the
// tenant the start command and JVM flags (R40-04). The backup leaves it out by
// identity; the seed must too.
func TestOverlaySeedSkipsAHardLinkToANodeFile(t *testing.T) {
	for _, sub := range []string{"", "survival"} {
		t.Run("sub="+sub, func(t *testing.T) {
			root, cmd := restoreArchive(t, []tar.Header{
				{Name: "world/r.0.0.mca", Mode: 0o644, Typeflag: tar.TypeReg},
			}, map[string]string{"world/r.0.0.mca": "restored region"})
			cmd.SubServer, cmd.Overlay = sub, true
			server := filepath.Join(root, cmd.ServerUUID)
			cfg := filepath.Join(server, ".node_config.json")
			if err := os.WriteFile(cfg, []byte(`{"startup":"java -Xmx4G"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(server, sub)
			if err := os.MkdirAll(filepath.Join(target, "plugins"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(cfg, filepath.Join(target, "plugins", "x")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "plugins", "own.yml"), []byte("mine"), 0o644); err != nil {
				t.Fatal(err)
			}
			rdb := newMiniRedis(t)
			report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
				RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
			})
			if report["status"] != "success" {
				t.Fatalf("report = %v", report)
			}
			if _, err := os.Lstat(filepath.Join(target, "plugins", "x")); err == nil {
				t.Error("a second name of .node_config.json was copied into the restored tree")
			}
			if b, err := os.ReadFile(filepath.Join(target, "plugins", "own.yml")); err != nil || string(b) != "mine" {
				t.Errorf("the tenant's own file = %q, %v", b, err)
			}
			if b, err := os.ReadFile(filepath.Join(server, ".node_config.json")); err != nil || string(b) == "" {
				t.Errorf(".node_config.json after the restore = %q, %v", b, err)
			}
		})
	}
}
