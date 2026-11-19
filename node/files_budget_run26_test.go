package main

import (
	"os"
	"path/filepath"
	"testing"

	beampb "dylaris-proto/beam"
	nodepb "dylaris-proto/node"
)

// R26-07: a failed copy removes what it wrote, but its budget still booked the
// bytes as landed and noted, so every write already running on the filesystem
// and the server's next uploads were refused for bytes no longer there.
func TestAFailedCopyBooksNothingAsLanded(t *testing.T) {
	for _, tc := range []struct {
		name, src, dst string
		overwrite      bool
	}{
		{"new file", "survival/world.dat", "survival/copy.dat", false},
		{"overwrite", "survival/world.dat", "survival/server.properties", true},
		{"folder", "survival", "creative", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, uuid, root := seedNodeOwned(t)
			if err := os.WriteFile(filepath.Join(root, "survival", "world.dat"), make([]byte, 64<<10), 0o644); err != nil {
				t.Fatal(err)
			}
			withDiskBudget(t, 16<<10, 1000)
			prev := serverDiskHeadroom
			serverDiskHeadroom = func(string) (string, int64, bool) { return uuid, 1 << 40, true }
			t.Cleanup(func() { serverDiskHeadroom = prev })

			disk := writesOn("")
			l0, n0 := disk.landed.Load(), notedFor(uuid).Load()
			if resp := h.handleCopy("c", uuid, &nodepb.CopyFileReq{SrcPath: tc.src, DstPath: tc.dst, Overwrite: tc.overwrite}); resp.GetError() == nil {
				t.Fatal("a copy larger than the free space succeeded")
			}
			if got := disk.landed.Load() - l0; got != 0 {
				t.Errorf("%d removed bytes booked as landed", got)
			}
			if got := notedFor(uuid).Load() - n0; got != 0 {
				t.Errorf("%d removed bytes noted on the server", got)
			}
			if g, w := disk.inflight.Load(), disk.onDisk.Load(); g != 0 || w != 0 {
				t.Errorf("still held: inflight %d, onDisk %d", g, w)
			}
		})
	}
}

// R26-06: a running Beam or mesh upload reserved its declared size and never
// marked what it streamed as written, so those bytes counted twice - out of
// the measured free space and still reserved - and one large upload refused
// every other write on the filesystem.
func TestStreamedUploadBytesAreMarkedWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, mid func()) // mid runs after 200 bytes streamed, before the end
	}{
		{"beam", func(t *testing.T, mid func()) {
			bs, _, ctx := newTestBeamServer(t)
			s := &midUploadStream{fakeBeamUploadStream: &fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "w.zip", 500, make([]byte, 120), make([]byte, 80))}, mid: mid}
			if err := bs.UploadFile(s); err != nil {
				t.Fatal(err)
			}
		}},
		{"mesh", func(t *testing.T, mid func()) {
			m, uuid := meshForBudget(t)
			cc := &coreConnection{stream: &fakeCoreStream{}}
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", ServerUuid: uuid, Payload: &nodepb.NodeMessage_WriteReq{WriteReq: &nodepb.WriteFileReq{Path: "survival/w.bin", TotalSize: 500}}})
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", Payload: &nodepb.NodeMessage_Chunk{Chunk: &nodepb.DataChunk{Data: make([]byte, 120)}}})
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", Payload: &nodepb.NodeMessage_Chunk{Chunk: &nodepb.DataChunk{Data: make([]byte, 80), Offset: 120}}})
			mid()
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", Payload: &nodepb.NodeMessage_TransferDone{TransferDone: &nodepb.TransferDone{TotalBytes: 200}}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDiskBudget(t, 1000, 1000)
			disk := writesOn("")
			called := false
			tc.run(t, func() {
				called = true
				if got := disk.onDisk.Load(); got != 200 {
					t.Errorf("%d of 200 streamed bytes marked as on the disk", got)
				}
			})
			if !called {
				t.Fatal("the upload never reached its middle")
			}
			if g, w := disk.inflight.Load(), disk.onDisk.Load(); g != 0 || w != 0 {
				t.Errorf("still held after the upload: inflight %d, onDisk %d", g, w)
			}
		})
	}
}

// midUploadStream runs mid once every message was received, before the EOF.
type midUploadStream struct {
	*fakeBeamUploadStream
	mid func()
}

func (s *midUploadStream) Recv() (*beampb.BeamUploadMsg, error) {
	if s.idx == len(s.msgs) && s.mid != nil {
		s.mid()
		s.mid = nil
	}
	return s.fakeBeamUploadStream.Recv()
}
