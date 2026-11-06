package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "dylaris-proto/node"
)

// .dylaris-backups sits inside the server directory, and that directory is
// bind-mounted into the tenant's own MC container at /data. The file API
// refuses to write through a ".dylaris-" path; a plugin in that container does
// not use the file API. A link planted there was followed by Stat and Open, so
// the download RPC streamed the link's target out through the panel with the
// node's own credentials - .node_secret and every other tenant's files
// included.
func TestBackupDownloadRefusesAPlantedSymlink(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	h := &StreamHandler{storageMgr: sm}
	const uuid = "srv-symlink-read"

	dir, err := h.resolveBackupDir(uuid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	secret := filepath.Join(t.TempDir(), "node_secret")
	if err := os.WriteFile(secret, []byte("THE-NODE-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, secret, filepath.Join(dir, "loot.tar.gz"))

	// A real archive beside it, so the guard is shown to reject the link
	// specifically rather than to break downloads.
	if err := os.WriteFile(filepath.Join(dir, "real.tar.gz"), []byte("ARCHIVE"), 0o644); err != nil {
		t.Fatal(err)
	}

	var sent []*pb.NodeMessage
	send := func(m *pb.NodeMessage) error { sent = append(sent, m); return nil }

	h.streamBackupArchive("r1", uuid, &pb.BackupOpenReq{Key: "loot.tar.gz"}, send)
	for _, m := range sent {
		if c := m.GetChunk(); c != nil && strings.Contains(string(c.Data), "THE-NODE-SECRET") {
			t.Fatal("the node secret was streamed out through the backup download")
		}
	}
	if len(sent) != 1 || sent[0].GetError() == nil {
		t.Fatalf("want a single error response, got %d messages", len(sent))
	}

	sent = nil
	h.streamBackupArchive("r2", uuid, &pb.BackupOpenReq{Key: "real.tar.gz"}, send)
	got := ""
	for _, m := range sent {
		if c := m.GetChunk(); c != nil {
			got += string(c.Data)
		}
	}
	if got != "ARCHIVE" {
		t.Errorf("a genuine archive no longer downloads: %q", got)
	}
}

// A link must not be advertised as a downloadable archive either, and its
// Lstat size must not land in the usage figure.
func TestBackupListAndUsageIgnoreNonFiles(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	h := &StreamHandler{storageMgr: sm}
	const uuid = "srv-symlink-list"

	dir, err := h.resolveBackupDir(uuid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, target, filepath.Join(dir, "link.tar.gz"))
	if err := os.MkdirAll(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.tar.gz"), []byte("ABC"), 0o644); err != nil {
		t.Fatal(err)
	}

	list := h.handleBackupList("r1", uuid).GetBackupListResp()
	if list == nil {
		t.Fatal("no list response")
	}
	if len(list.Objects) != 1 || list.Objects[0].Key != "real.tar.gz" {
		keys := []string{}
		for _, o := range list.Objects {
			keys = append(keys, o.Key)
		}
		t.Errorf("listed %v, want only real.tar.gz", keys)
	}

	usage := h.handleBackupUsage("r2", uuid).GetBackupUsageResp()
	if usage == nil {
		t.Fatal("no usage response")
	}
	if usage.Count != 1 || usage.UsedBytes != 3 {
		t.Errorf("usage = %d bytes / %d files, want 3 / 1", usage.UsedBytes, usage.Count)
	}
}

// The write side of the same directory: a link planted under the name the next
// run is about to use would have been followed by O_CREATE|O_TRUNC, writing the
// archive wherever it points.
func TestNodeLocalUploadRefusesToWriteThroughASymlink(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	const uuid = "srv-symlink-write"
	dir := filepath.Join(resolveServerRoot(sm, uuid), backupDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, victim, filepath.Join(dir, "run-7.tar.gz"))

	info := storageInfo{Provider: "node-local", Config: json.RawMessage(`{}`)}
	err := uploadBackup(t.Context(), sm, uuid, info, "backups/"+uuid+"/job-1/run-7.tar.gz",
		strings.NewReader("ARCHIVE-BYTES"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	if got, rerr := os.ReadFile(victim); rerr != nil || string(got) != "ORIGINAL" {
		t.Fatalf("the archive was written through the link: victim = %q (%v)", got, rerr)
	}
	got, rerr := os.ReadFile(filepath.Join(dir, "run-7.tar.gz"))
	if rerr != nil || string(got) != "ARCHIVE-BYTES" {
		t.Fatalf("the archive did not land in the backup dir: %q (%v)", got, rerr)
	}
}
