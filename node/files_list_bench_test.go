package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

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
