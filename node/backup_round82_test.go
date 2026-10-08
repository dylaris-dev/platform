package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/queue"
	pb "dylaris-proto/node"
)

// A tenant's own bucket answered the restore GET and then trickled nothing:
// the restore held one of this node's eight command slots for good.
func TestARestoreGivesUpOnAStalledDownload(t *testing.T) {
	prevStall, prevNode, prevReq := restoreStallTimeout, nodeID, coreRequest
	t.Cleanup(func() { restoreStallTimeout, nodeID, coreRequest = prevStall, prevNode, prevReq })
	restoreStallTimeout = 200 * time.Millisecond
	nodeID = "node-r82"

	release := make(chan struct{})
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer src.Close()
	defer close(release)
	coreRequest = func(context.Context, *pb.NodeMessage, time.Duration) (*pb.NodeMessage, error) {
		return &pb.NodeMessage{Payload: &pb.NodeMessage_RestoreUrlResponse{RestoreUrlResponse: &pb.RestoreUrlResponse{Url: src.URL}}}, nil
	}

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "srv-r82"), 0o755)
	blob, _ := json.Marshal(storageInfo{ID: 1, Provider: "s3"})
	cmd := BackupRestoreCommand{RunID: 1, RestoreID: 82, ServerUUID: "srv-r82", StorageKey: "k", Storage: blob, Download: modeDownloadPresigned}
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		done := make(chan struct{})
		go func() {
			RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a stalled restore download held the command")
		}
	})
	if report["status"] != "failed" {
		t.Fatalf("report = %v, want failed", report)
	}
}

// Two restores of one server, whatever their ids, must not run side by side.
func TestASecondRestoreOfTheSameServerIsRefused(t *testing.T) {
	root, cmd := restoreArchive(t, []tar.Header{{Name: "a.txt", Mode: 0o644, Typeflag: tar.TypeReg}}, map[string]string{"a.txt": "new"})
	restoreServersInFlight.enter(cmd.ServerUUID)
	defer restoreServersInFlight.leave(cmd.ServerUUID)
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "failed" || !strings.Contains(report["error"].(string), "another restore") {
		t.Fatalf("report = %v", report)
	}
	if got, _ := os.ReadFile(filepath.Join(root, cmd.ServerUUID, "keep.txt")); string(got) != "before" {
		t.Fatal("the server was replaced under the running restore")
	}
}

// Entries cost inodes and blocks that a byte budget over file contents never
// counted: millions of empty directories exhaust a shared filesystem.
func TestARestoreCountsEntriesAgainstTheBudget(t *testing.T) {
	dirs := []tar.Header{
		{Name: "d1/", Mode: 0o755, Typeflag: tar.TypeDir},
		{Name: "d2/", Mode: 0o755, Typeflag: tar.TypeDir},
		{Name: "d3/", Mode: 0o755, Typeflag: tar.TypeDir},
		{Name: "a.txt", Mode: 0o644, Typeflag: tar.TypeReg},
	}
	t.Run("entry cap", func(t *testing.T) {
		prev := maxRestoreEntries
		maxRestoreEntries = 2
		t.Cleanup(func() { maxRestoreEntries = prev })
		restoreFails(t, dirs)
	})
	t.Run("entry cost", func(t *testing.T) {
		prev := restoreDiskBudget
		restoreDiskBudget = func(string) int64 { return 3 * restoreEntryCost }
		t.Cleanup(func() { restoreDiskBudget = prev })
		restoreFails(t, dirs)
	})
}

// MkdirAll creates the parents an entry names without an entry of their own:
// one file at the end of a deep path was thousands of directories for the
// price of one.
func TestARestoreCountsTheParentsItCreates(t *testing.T) {
	prev := maxRestoreEntries
	maxRestoreEntries = 4
	t.Cleanup(func() { maxRestoreEntries = prev })
	restoreFails(t, []tar.Header{{Name: "a/b/c/d/e/f.txt", Mode: 0o644, Typeflag: tar.TypeReg}})
}

func restoreFails(t *testing.T, entries []tar.Header) {
	t.Helper()
	root, cmd := restoreArchive(t, entries, map[string]string{"a.txt": "x"})
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "failed" {
		t.Fatalf("report = %v, want failed", report)
	}
}

// A stash kept because it holds what could not be carried must leave the
// cleanup's pattern, or the cleanup deletes it a day later.
func TestAKeptStashLeavesTheCleanupsReach(t *testing.T) {
	dir := t.TempDir()
	stash := filepath.Join(dir, "srv.pre-restore-20261008-120000")
	os.MkdirAll(stash, 0o755)
	keepStash(1, stash)
	entries, _ := os.ReadDir(dir)
	// Hidden too: listings that skip dot-directories must not take it for a
	// sub-server or a server.
	if len(entries) != 1 || preRestoreStash.MatchString(entries[0].Name()) || !strings.HasPrefix(entries[0].Name(), ".") {
		t.Fatalf("after keepStash: %v", entries)
	}
	keepStash(1, filepath.Join(dir, "absent.pre-restore-20261008-120000")) // must not panic or create anything
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("keepStash of a missing stash changed the directory: %v", entries)
	}
}
