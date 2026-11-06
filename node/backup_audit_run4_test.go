package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/queue"
	pb "dylaris-proto/node"
)

func TestRestoreDeadlineScalesWithTheArchive(t *testing.T) {
	tests := []struct {
		size int64
		want time.Duration
	}{
		{-1, restoreDeadlineUnknownSize},
		{0, restoreDeadlineFloor},
		{restoreDeadlineMinRate * 3600, restoreDeadlineFloor + time.Hour},
	}
	for _, tc := range tests {
		if got := restoreDeadline(tc.size); got != tc.want {
			t.Errorf("restoreDeadline(%d) = %v, want %v", tc.size, got, tc.want)
		}
	}
}

// The stall guard is a minimum rate, and a manifest entry's body is skipped
// without a write, so no budget saw it either: a tenant's endpoint serving one
// endless entry fast enough held the restore, and its command slot, for good.
func TestARestoreEndsAtItsDeadlineHoweverFastTheSourceSends(t *testing.T) {
	prevDL, prevNode, prevReq := restoreDeadlineUnknownSize, nodeID, coreRequest
	t.Cleanup(func() { restoreDeadlineUnknownSize, nodeID, coreRequest = prevDL, prevNode, prevReq })
	restoreDeadlineUnknownSize = 500 * time.Millisecond
	nodeID = "node-j23"

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // no Content-Length: chunked, size unknown
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: ".dylaris/backup.json", Mode: 0o644, Size: 1 << 50, Typeflag: tar.TypeReg})
		buf := make([]byte, 64<<10)
		for r.Context().Err() == nil {
			if _, err := tw.Write(buf); err != nil {
				return
			}
			gz.Flush()
			w.(http.Flusher).Flush()
		}
	}))
	defer src.Close()
	coreRequest = func(context.Context, *pb.NodeMessage, time.Duration) (*pb.NodeMessage, error) {
		return &pb.NodeMessage{Payload: &pb.NodeMessage_RestoreUrlResponse{RestoreUrlResponse: &pb.RestoreUrlResponse{Url: src.URL}}}, nil
	}

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "srv-j23"), 0o755)
	blob, _ := json.Marshal(storageInfo{ID: 1, Provider: "s3"})
	cmd := BackupRestoreCommand{RunID: 1, RestoreID: 123, ServerUUID: "srv-j23", StorageKey: "k", Storage: blob, Download: modeDownloadPresigned}
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		done := make(chan struct{})
		go func() {
			RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("an endless restore stream held the command past its deadline")
		}
	})
	if report["status"] != "failed" {
		t.Fatalf("report = %v, want failed", report)
	}
}

// The restore holds "restarting" while it runs. A stopped server is not started
// again and nothing else reports a container that is not running, so the status
// stayed "restarting" and Core refused Start on the restored server.
func TestARestoreOfAStoppedServerEndsStopped(t *testing.T) {
	tests := []struct {
		name    string
		entries []tar.Header
		status  string
	}{
		{"success", []tar.Header{{Name: "a.txt", Mode: 0o644, Typeflag: tar.TypeReg}}, "success"},
		{"failure", []tar.Header{{Name: "d/", Mode: 0o755, Typeflag: tar.TypeDir}}, "failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, cmd := restoreArchive(t, tc.entries, map[string]string{"a.txt": "new"})
			rdb := newMiniRedis(t)
			report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
				RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
			})
			if report["status"] != tc.status {
				t.Fatalf("report = %v, want %s", report, tc.status)
			}
			key := fmt.Sprintf("dylaris:server:%s:status", cmd.ServerUUID)
			if got := rdb.Get(context.Background(), key).Val(); got != "stopped" {
				t.Fatalf("status after the restore = %q, want stopped", got)
			}
		})
	}
}

// Only the backup writer creates .dylaris-backups. The read and delete RPCs
// created it, so Core's archive purge right after a server delete put the
// deleted server's directory back on disk, where it showed as an orphan.
func TestBackupRPCsDoNotCreateTheServerDirectory(t *testing.T) {
	base := t.TempDir()
	h := &StreamHandler{storageMgr: NewStorageManager(base, nil)}
	const uuid = "srv-j25-gone"
	send := func(*pb.NodeMessage) error { return nil }

	h.handleBackupList("r1", uuid)
	h.handleBackupUsage("r2", uuid)
	if e := h.handleBackupDelete("r3", uuid, &pb.BackupDeleteReq{Key: "a.tar.gz"}).GetError(); e == nil || e.Code != 404 {
		t.Errorf("delete of a missing archive = %v, want 404", e)
	}
	h.streamBackupArchive("r4", uuid, &pb.BackupOpenReq{Key: "a.tar.gz"}, send)
	if _, err := os.Lstat(filepath.Join(base, uuid)); !os.IsNotExist(err) {
		t.Fatalf("a backup RPC created the server directory (err=%v)", err)
	}
}

// The spool decision measured the whole tree, the server's own node-local
// archives included, which the archive never carries.
func TestArchiveSourceBytesLeavesOutTheBackupStores(t *testing.T) {
	dir := t.TempDir()
	for name, size := range map[string]int{
		"world/level.dat":                1000,
		backupDirName + "/old.tar.gz":    5000,
		manifestDirName + "/backup.json": 300,
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// du counts directory blocks too; the files are what must not count.
	if got := archiveSourceBytes(dir); got < 1000 || got >= 1300 {
		t.Fatalf("archiveSourceBytes = %d, want the world's 1000 bytes", got)
	}
}

// A directory removed after its parent was listed failed the whole walk, and
// with it a backup of a live tree.
func TestWalkRootSkipsADirectoryRemovedMidWalk(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a"), 0o755)
	os.MkdirAll(filepath.Join(dir, "b", "sub"), 0o755)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = walkRoot(root, ".", func(name string, _ fs.FileInfo) error {
		if name == "a" {
			return os.RemoveAll(filepath.Join(dir, "b"))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walkRoot = %v, want the vanished directory skipped", err)
	}
	if err := walkRoot(root, "missing", func(string, fs.FileInfo) error { return nil }); err == nil {
		t.Fatal("a missing start must still fail the walk")
	}
}
