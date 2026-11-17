package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "dylaris-proto/node"
)

// BenchmarkHandleList lists a folder of 1,000 entries: 950 files and 50
// folders whose sizes are summed from 20 files each, two levels down, plus a
// backup store the listing has to recognise by identity.
func BenchmarkHandleList(b *testing.B) {
	// The storage manager logs on creation, which splits the benchmark line.
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	sm := NewStorageManager(b.TempDir(), nil)
	h := NewStreamHandler(sm)
	const uuid = "86868686-8686-8686-8686-868686868686"
	dir := filepath.Join(sm.GetServerDir(uuid), "survival")
	write := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < 950; i++ {
		write(filepath.Join(dir, fmt.Sprintf("file-%04d.dat", i)))
	}
	for i := 0; i < 50; i++ {
		for j := 0; j < 20; j++ {
			write(filepath.Join(dir, fmt.Sprintf("dir-%02d", i), "region", fmt.Sprintf("r.%d.mca", j)))
		}
	}
	for i := 0; i < 20; i++ {
		write(filepath.Join(sm.GetServerDir(uuid), ".dylaris-backups", "job-1", fmt.Sprintf("b%02d.tar.gz", i)))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		msg := h.handleList("l", uuid, &pb.ListFilesReq{Path: "survival"})
		if n := len(msg.GetListResp().GetFiles()); n != 1000 {
			b.Fatalf("listed %d entries, want 1000: %v", n, msg.GetError())
		}
	}
}

// A folder's size is the sum of the files under it at any depth, and a link
// counts as itself rather than as what it points at.
func TestListingSumsFolderSizes(t *testing.T) {
	h, uuid, root := seedNodeOwned(t)
	big := filepath.Join(root, "outside.bin")
	if err := os.WriteFile(big, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(big, filepath.Join(root, "survival", "plugins", "link")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	link, err := os.Lstat(filepath.Join(root, "survival", "plugins", "link"))
	if err != nil {
		t.Fatal(err)
	}

	msg := h.handleList("l", uuid, &pb.ListFilesReq{Path: "."})
	for _, f := range msg.GetListResp().GetFiles() {
		if f.Name != "survival" {
			continue
		}
		// server.properties (7) + plugins/ok.jar (3) + the link itself.
		if want := 7 + 3 + link.Size(); f.Size != want {
			t.Fatalf("survival lists %d bytes, want %d", f.Size, want)
		}
		return
	}
	t.Fatalf("survival not listed: %v", msg)
}

// A folder size is reused within dirSizeTTL, recomputed after it, and dropped
// for the server when the node itself changes a file there.
func TestListingCachesFolderSizes(t *testing.T) {
	h, uuid, root := seedNodeOwned(t)
	clock := time.Unix(1000, 0)
	nowDirSize = func() time.Time { return clock }
	t.Cleanup(func() { nowDirSize = time.Now })

	survivalSize := func() int64 {
		t.Helper()
		for _, f := range h.handleList("l", uuid, &pb.ListFilesReq{Path: "."}).GetListResp().GetFiles() {
			if f.Name == "survival" {
				return f.Size
			}
		}
		t.Fatal("survival not listed")
		return 0
	}
	grow := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "survival", name), make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if got := survivalSize(); got != 10 {
		t.Fatalf("first listing: %d, want 10", got)
	}
	grow("a.bin") // written behind the node's back, as the game server does
	if got := survivalSize(); got != 10 {
		t.Fatalf("within the TTL: %d, want the cached 10", got)
	}
	clock = clock.Add(dirSizeTTL + time.Second)
	if got := survivalSize(); got != 110 {
		t.Fatalf("after the TTL: %d, want 110", got)
	}

	grow("b.bin")
	h.Handle(&pb.NodeMessage{RequestId: "c", ServerUuid: uuid, Payload: &pb.NodeMessage_CreateReq{CreateReq: &pb.CreateFileReq{Path: "survival/new.txt"}}})
	if got := survivalSize(); got != 210 {
		t.Fatalf("after a node write: %d, want 210", got)
	}
}

// An upload streams into a staging file for as long as Core takes to send it,
// and a listing in that window caches the partial size. The commit is the
// write the panel waits for, so it drops the cache; the WriteReq that opened
// the upload came too early to.
func TestAnUploadCommitDropsTheFolderSizes(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	clock := time.Unix(1000, 0)
	nowDirSize = func() time.Time { return clock }
	t.Cleanup(func() { nowDirSize = time.Now })

	survivalSize := func() int64 {
		t.Helper()
		for _, f := range h.handleList("l", uuid, &pb.ListFilesReq{Path: "."}).GetListResp().GetFiles() {
			if f.Name == "survival" {
				return f.Size
			}
		}
		t.Fatal("survival not listed")
		return 0
	}

	f, tmp, err := h.createUploadTemp(uuid, "survival/world.zip")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(make([]byte, 40))
	// The partial size itself is the OS's to report (NTFS updates it lazily);
	// what matters is that this listing caches something.
	survivalSize()
	f.Write(make([]byte, 60))
	f.Close()
	if _, err := h.commitUpload(uuid, "survival/world.zip", tmp); err != nil {
		t.Fatal(err)
	}
	if got := survivalSize(); got != 110 {
		t.Fatalf("after the commit: %d, want 110", got)
	}
}

// A walk that started before a forget measured the tree as it was; storing
// it afterwards would undo the forget for a whole TTL.
func TestAWalkFromBeforeAForgetIsNotCached(t *testing.T) {
	c := &dirSizeCache{m: map[string]map[string]dirSizeEntry{}}
	got := c.get("/srv/a", "survival", func() int64 {
		c.forget("/srv/a") // a delete lands while the walk runs
		return 10
	})
	if got != 10 {
		t.Fatalf("the walk's own caller got %d, want 10", got)
	}
	if got := c.get("/srv/a", "survival", func() int64 { return 99 }); got != 99 {
		t.Fatalf("after the forget: %d, want a fresh 99", got)
	}
	if got := c.get("/srv/a", "survival", func() int64 { return 7 }); got != 99 {
		t.Fatalf("an undisturbed walk: %d, want the cached 99", got)
	}
}
