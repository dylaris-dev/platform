package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	beampb "dylaris-proto/beam"
	pb "dylaris-proto/node"
)

// world/playerdata on a server with 1,500 players: a .dat and a .dat_old each,
// about 147 KB as one ListFilesResp, past the 128 KB Core accepts. A paged
// listing arrives in parts Core takes; an unpaged one (an older Core) is one
// message, as before.
func TestAPagedListingFitsCoresLimit(t *testing.T) {
	h, uuid, root := seedNodeOwned(t)
	dir := filepath.Join(root, "survival", "world", "playerdata")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const players = 1500
	for i := range players {
		name := fmt.Sprintf("%08x-0000-4000-8000-%012x.dat", i, i)
		for _, n := range []string{name, name + "_old"} {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("NBT"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	list := func(paged bool) []*pb.NodeMessage {
		return h.Handle(&pb.NodeMessage{RequestId: "ls", ServerUuid: uuid, Payload: &pb.NodeMessage_ListReq{ListReq: &pb.ListFilesReq{Path: "survival/world/playerdata", Paged: paged}}})
	}

	for _, tc := range []struct {
		name      string
		paged     bool
		wantParts func(int) bool
	}{
		{"paged", true, func(n int) bool { return n > 1 }},
		{"unpaged", false, func(n int) bool { return n == 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs := list(tc.paged)
			if !tc.wantParts(len(msgs)) {
				t.Fatalf("%d messages", len(msgs))
			}
			seen := map[string]bool{}
			for i, m := range msgs {
				resp := m.GetListResp()
				if resp == nil {
					t.Fatalf("part %d is not a listing: %v", i, m)
				}
				if m.RequestId != "ls" {
					t.Errorf("part %d request id %q", i, m.RequestId)
				}
				if want := i < len(msgs)-1; resp.More != want {
					t.Errorf("part %d more = %v, want %v", i, resp.More, want)
				}
				if tc.paged {
					if n := proto.Size(m); n > coreMaxMessage {
						t.Errorf("part %d is %d bytes, over Core's limit", i, n)
					}
				}
				for _, f := range resp.Files {
					seen[f.Name] = true
				}
			}
			if len(seen) != 2*players {
				t.Errorf("%d distinct entries, want %d", len(seen), 2*players)
			}
		})
	}
}

// An empty folder paged is still one reply, so Core is not left waiting.
func TestAPagedEmptyListingIsOneReply(t *testing.T) {
	h, uuid, root := seedNodeOwned(t)
	if err := os.MkdirAll(filepath.Join(root, "survival", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	msgs := h.Handle(&pb.NodeMessage{RequestId: "ls", ServerUuid: uuid, Payload: &pb.NodeMessage_ListReq{ListReq: &pb.ListFilesReq{Path: "survival/empty", Paged: true}}})
	if len(msgs) != 1 || msgs[0].GetListResp() == nil || msgs[0].GetListResp().More {
		t.Fatalf("got %v", msgs)
	}
}

// New file, copy and rename onto a name that already exists used to replace
// it and report success: "New file" named server.properties emptied it.
func TestFileOpsRefuseAnExistingTarget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		run      func(h *StreamHandler, uuid string) *pb.NodeMessage
		wantCode int32
		wantBody string
	}{
		{"create", func(h *StreamHandler, uuid string) *pb.NodeMessage {
			return h.handleCreate("c", uuid, &pb.CreateFileReq{Path: "survival/server.properties"})
		}, 409, "motd=hi"},
		{"copy", func(h *StreamHandler, uuid string) *pb.NodeMessage {
			return h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival/plugins/ok.jar", DstPath: "survival/server.properties"})
		}, 409, "motd=hi"},
		{"copy asked to overwrite", func(h *StreamHandler, uuid string) *pb.NodeMessage {
			return h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival/plugins/ok.jar", DstPath: "survival/server.properties", Overwrite: true})
		}, 0, "JAR"},
		{"rename", func(h *StreamHandler, uuid string) *pb.NodeMessage {
			return h.handleRename("c", uuid, &pb.RenameFileReq{OldPath: "survival/plugins/ok.jar", NewName: "x"})
		}, 409, "motd=hi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, uuid, root := seedNodeOwned(t)
			target := filepath.Join(root, "survival", "server.properties")
			if tc.name == "rename" {
				// Same folder as the source: rename keeps the parent.
				target = filepath.Join(root, "survival", "plugins", "x")
				os.WriteFile(target, []byte("motd=hi"), 0o644)
			}
			msg := tc.run(h, uuid)
			if got := msg.GetError().GetCode(); got != tc.wantCode {
				t.Fatalf("code %d (%v), want %d", got, msg, tc.wantCode)
			}
			if b, _ := os.ReadFile(target); string(b) != tc.wantBody {
				t.Errorf("target holds %q, want %q", b, tc.wantBody)
			}
		})
	}
}

// A new name still works for all three.
func TestFileOpsWriteANewTarget(t *testing.T) {
	h, uuid, root := seedNodeOwned(t)
	for _, msg := range []*pb.NodeMessage{
		h.handleCreate("c", uuid, &pb.CreateFileReq{Path: "survival/new.txt"}),
		h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival/plugins/ok.jar", DstPath: "survival/copy.jar"}),
		h.handleRename("c", uuid, &pb.RenameFileReq{OldPath: "survival/copy.jar", NewName: "moved.jar"}),
	} {
		if e := msg.GetError(); e != nil {
			t.Fatalf("refused: %v", e)
		}
	}
	for _, n := range []string{"new.txt", "moved.jar"} {
		if _, err := os.Stat(filepath.Join(root, "survival", n)); err != nil {
			t.Error(err)
		}
	}
}

// Beam's file browser is the same component and gets the same answer.
func TestBeamFileOpsRefuseAnExistingTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(bs *beamServer, ctx context.Context) (*beampb.BeamOpResp, error)
	}{
		{"create", func(bs *beamServer, ctx context.Context) (*beampb.BeamOpResp, error) {
			return bs.CreateFile(ctx, &beampb.BeamFileCreateReq{Path: "taken.txt"})
		}},
		{"copy", func(bs *beamServer, ctx context.Context) (*beampb.BeamOpResp, error) {
			return bs.CopyFile(ctx, &beampb.BeamFileCopyReq{SrcPath: "src.txt", DstPath: "taken.txt"})
		}},
		{"rename", func(bs *beamServer, ctx context.Context) (*beampb.BeamOpResp, error) {
			return bs.RenameFile(ctx, &beampb.BeamFileRenameReq{OldPath: "src.txt", NewName: "taken.txt"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs, uuid, ctx := newTestBeamServer(t)
			dir := bs.storageMgr.GetServerDir(uuid)
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, "src.txt"), []byte("src"), 0o644)
			os.WriteFile(filepath.Join(dir, "taken.txt"), []byte("keep"), 0o644)
			resp, err := tc.run(bs, ctx)
			if err != nil || resp.Success {
				t.Fatalf("resp %v err %v, want a refusal", resp, err)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "taken.txt")); string(b) != "keep" {
				t.Errorf("target holds %q", b)
			}
		})
	}
}

// An upload whose Core connection dropped mid-transfer held its whole declared
// size for up to six minutes, so the retry was refused with 413. The
// connection's teardown now ends its uploads; another Core's are kept.
func TestADroppedConnectionReleasesItsUploads(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	withDiskBudget(t, 1<<30, 1000)
	_, rdb := gaugeRedis(t)
	rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":0,"limit":1000}`, 10*time.Minute)
	gone := &coreConnection{stream: &fakeCoreStream{}}
	other := &coreConnection{stream: &fakeCoreStream{}}
	m := &MeshManager{handler: h, rdb: rdb, pendingWrites: map[string]*pendingWrite{}}

	m.handleRequest(gone, &pb.NodeMessage{RequestId: "a", ServerUuid: uuid, Payload: &pb.NodeMessage_WriteReq{WriteReq: &pb.WriteFileReq{Path: "survival/a.bin", TotalSize: 300}}})
	m.handleRequest(other, &pb.NodeMessage{RequestId: "b", ServerUuid: uuid, Payload: &pb.NodeMessage_WriteReq{WriteReq: &pb.WriteFileReq{Path: "survival/b.bin", TotalSize: 200}}})
	m.dropConnectionWrites(gone)

	if _, ok := m.pendingWrites["a"]; ok {
		t.Error("the dropped connection's upload is still pending")
	}
	if _, ok := m.pendingWrites["b"]; !ok {
		t.Fatal("another connection's upload was dropped")
	}
	if n := inflightWrites.Load(); n != 200 {
		t.Errorf("%d bytes reserved, want only the live upload's 200", n)
	}
	m.handleRequest(other, &pb.NodeMessage{RequestId: "b", Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{}}})
}
