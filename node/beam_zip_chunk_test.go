package main

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	pb "dylaris-proto/beam"
)

// A streamed zip goes through an io.Pipe, and a pipe Read returns what ONE
// write delivered: deflate output arrived in ~4 KiB writes, so every gRPC
// message carried 4 KiB instead of a chunk, and the per-message cost held a
// 10 Gbit LAN download near 40 MB/s. Every chunk but the last must be full.
func TestStreamZipSendsFullChunks(t *testing.T) {
	bs, serverUUID, ctx := newTestBeamServer(t)
	dir := filepath.Join(bs.storageMgr.GetServerDir(serverUUID), "world")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	region := make([]byte, 3<<20)
	rand.Read(region)
	text := make([]byte, 3<<20)
	for i := range text {
		text[i] = "abcdefgh\n"[i%9]
	}
	for name, body := range map[string][]byte{"r.0.0.mca": region, "latest.log": text} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	st := &fakeBeamDownloadStream{ctx: ctx}
	if err := bs.DownloadFile(&pb.BeamDownloadReq{Path: "world", ZipIfDir: true}, st); err != nil {
		t.Fatal(err)
	}
	if len(st.chunks) < 2 {
		t.Fatalf("only %d chunks", len(st.chunks))
	}
	for i, c := range st.chunks[:len(st.chunks)-1] {
		if len(c.Data) != beamChunkSize {
			t.Fatalf("chunk %d of %d carries %d bytes, want a full %d", i, len(st.chunks), len(c.Data), beamChunkSize)
		}
	}
}
