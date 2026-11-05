package main

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"
)

// A delete ran beside a whole-server restore: the stage sat outside the removed
// directory and the swap put the deleted server's world back on disk.
func TestARestoreOfADeletedServerDoesNotBringItBack(t *testing.T) {
	var retagged []string
	prev := retagAfterRestore
	t.Cleanup(func() { retagAfterRestore = prev })
	retagAfterRestore = func(_ context.Context, _ *redis.Client, uuid string) { retagged = append(retagged, uuid) }

	root, cmd := restoreArchive(t, []tar.Header{
		{Name: "world/level.dat", Mode: 0o644, Typeflag: tar.TypeReg},
	}, map[string]string{"world/level.dat": "w"})
	prevDir := nodeSecretDir
	nodeSecretDir = ""
	t.Cleanup(func() { nodeSecretDir = prevDir; forgetTombstoneInMemory(cmd.ServerUUID) })
	// What the delete does to disk; RunRestore itself only sees the tombstone.
	markServerDeleted(cmd.ServerUUID)
	if err := os.RemoveAll(filepath.Join(root, cmd.ServerUUID)); err != nil {
		t.Fatal(err)
	}

	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "failed" || report["error"] != errRestoreServerDeleted {
		t.Fatalf("report = %v, want failed with %q", report, errRestoreServerDeleted)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), cmd.ServerUUID) {
			t.Errorf("%s is on disk after the delete", e.Name())
		}
	}
	if len(retagged) != 0 {
		t.Errorf("retagged %v, want no quota project for a deleted server", retagged)
	}
}

// A backup of a stopped sub-server turned saving off on the one that runs.
func TestSavingPausesOnlyForTheSubServerThatRuns(t *testing.T) {
	prev := saveFlushTimeout
	saveFlushTimeout = 10 * time.Millisecond
	t.Cleanup(func() { saveFlushTimeout = prev })
	for _, tc := range []struct {
		name, running, archived string
		wantPaused              bool
	}{
		{"the running sub-server", "/data/A", "A", true},
		{"another sub-server", "/data/A", "B", false},
		{"the whole server", "/data/A", "", true},
		{"a container without a sub-server", "/data", "B", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newMiniRedis(t)
			g := pauseWorldSaves(context.Background(), rdb, dockerWithWorkingDir(t, tc.running), "srv-1", tc.archived)
			if paused := g.send != nil; paused != tc.wantPaused {
				t.Fatalf("paused = %v, want %v", paused, tc.wantPaused)
			}
			queued, _ := rdb.LLen(context.Background(), "dylaris:server:srv-1:input").Result()
			if (queued > 0) != tc.wantPaused {
				t.Fatalf("%d commands queued, want some = %v", queued, tc.wantPaused)
			}
		})
	}
}

// A whole-server archive carried the node's 0600 files, so a member with
// backups.read downloaded the start command and JVM flags hidden from them.
func TestAnArchiveLeavesOutTheNodesHiddenFiles(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		".node_config.json":      `{"docker":{"extraJvmFlags":"-Dtoken=x"}}`,
		".dylaris.json":          "{}",
		netGateFile:              "",
		".active_server":         "main",
		"main/world.dat":         "w",
		"main/.node_config.json": "a tenant file of that name",
	} {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, dir string
		want      []string
	}{
		{"whole server", root, []string{".active_server", "main/.node_config.json", "main/world.dat"}},
		{"sub-server", filepath.Join(root, "main"), []string{".node_config.json", "world.dat"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := writeServerArchive(&buf, root, tc.dir, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			var got []string
			for n := range readArchive(t, buf.Bytes()) {
				got = append(got, n)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("archive = %v, want %v", got, tc.want)
			}
		})
	}
}
