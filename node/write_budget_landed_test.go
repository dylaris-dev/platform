package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"
	nodepb "dylaris-proto/node"

	"github.com/pkg/sftp"
)

// L26/L13: a write budget booked everything it had reserved as landed on the
// disk when it was released, so a refused or abandoned reservation of a
// declared size counted against every budget still running on the node - one
// Beam Start with a huge TotalSize made every SFTP upload, copy and extract in
// progress fail as out of space. Only what reached the disk may land.
func TestOnlyWrittenBytesLandOnTheNode(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T)
		want int64
	}{
		{"refused mesh upload", func(t *testing.T) {
			m, uuid := meshForBudget(t)
			cc := &coreConnection{stream: &fakeCoreStream{}}
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "r", ServerUuid: uuid, Payload: &nodepb.NodeMessage_WriteReq{WriteReq: &nodepb.WriteFileReq{Path: "survival/r.bin", TotalSize: 5000}}})
			if _, ok := m.pendingWrites["r"]; ok {
				t.Fatal("an upload past the node's free space was accepted")
			}
		}, 0},
		{"dropped mesh upload", func(t *testing.T) {
			m, uuid := meshForBudget(t)
			cc := &coreConnection{stream: &fakeCoreStream{}}
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "d", ServerUuid: uuid, Payload: &nodepb.NodeMessage_WriteReq{WriteReq: &nodepb.WriteFileReq{Path: "survival/d.bin", TotalSize: 300}}})
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "d", Payload: &nodepb.NodeMessage_Chunk{Chunk: &nodepb.DataChunk{Data: []byte("abcd")}}})
			m.dropConnectionWrites(cc)
		}, 0},
		{"mesh upload shorter than declared", func(t *testing.T) {
			m, uuid := meshForBudget(t)
			cc := &coreConnection{stream: &fakeCoreStream{}}
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "s", ServerUuid: uuid, Payload: &nodepb.NodeMessage_WriteReq{WriteReq: &nodepb.WriteFileReq{Path: "survival/s.bin", TotalSize: 300}}})
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "s", Payload: &nodepb.NodeMessage_Chunk{Chunk: &nodepb.DataChunk{Data: []byte("abcd")}}})
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "s", Payload: &nodepb.NodeMessage_TransferDone{TransferDone: &nodepb.TransferDone{TotalBytes: 4}}})
		}, 4},
		{"refused beam start", func(t *testing.T) {
			bs, _, ctx := newTestBeamServer(t)
			if err := bs.UploadFile(&fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "huge.zip", 1<<40)}); err == nil {
				t.Fatal("a start past the node's free space was accepted")
			}
		}, 0},
		{"aborted beam upload", func(t *testing.T) {
			bs, _, ctx := newTestBeamServer(t)
			s := &fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "a.zip", 500, []byte("abcd")), end: errors.New("stream broke")}
			if err := bs.UploadFile(s); err == nil {
				t.Fatal("a broken stream reported success")
			}
		}, 0},
		{"completed beam upload shorter than declared", func(t *testing.T) {
			bs, _, ctx := newTestBeamServer(t)
			if err := bs.UploadFile(&fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "c.zip", 500, []byte("abcdefgh"))}); err != nil {
				t.Fatal(err)
			}
		}, 8},
		{"refused beam save", func(t *testing.T) {
			bs, uuid, ctx := newTestBeamServer(t)
			os.MkdirAll(filepath.Join(bs.storageMgr.GetServerDir(uuid), "survival"), 0o755)
			resp, err := bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "survival/a.txt", Content: string(make([]byte, 5000))})
			if err != nil || resp.Success {
				t.Fatalf("a save past the node's free space: %v, %+v", err, resp)
			}
		}, 0},
		{"copy refused mid-read", func(t *testing.T) {
			b := nodeWriteBudget(t.TempDir())
			n, err := io.Copy(io.Discard, b.reader(bytes.NewReader(make([]byte, 2000))))
			if err == nil || n != 1000 {
				t.Fatalf("copy = %d, %v", n, err)
			}
			b.release()
		}, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDiskBudget(t, 1000, 1000)
			l0 := landedWrites.Load()
			tc.run(t)
			if got := landedWrites.Load() - l0; got != tc.want {
				t.Errorf("%d bytes booked as landed, want %d", got, tc.want)
			}
			if n := inflightWrites.Load(); n != 0 {
				t.Errorf("%d bytes still held", n)
			}
		})
	}
	// The sibling a phantom landing starved: an SFTP handle open across the
	// refused start must still write.
	t.Run("an open sftp handle survives a refused start", func(t *testing.T) {
		withDiskBudget(t, 1000, 1000)
		sm := NewStorageManager(t.TempDir(), nil)
		const uuid = "26262626-2626-2626-2626-262626262626"
		os.MkdirAll(filepath.Join(sm.GetServerDir(uuid), "survival"), 0o755)
		refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
		w, err := newVirtualFS(refs, sm, nil, "alice").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/survival/a.bin", Flags: 0x02 | 0x08 | 0x10})
		if err != nil {
			t.Fatal(err)
		}
		defer w.(io.Closer).Close()
		bs, _, ctx := newTestBeamServer(t)
		bs.UploadFile(&fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "huge.zip", 1<<40)})
		if _, err := w.WriteAt(make([]byte, 100), 0); err != nil {
			t.Errorf("the open handle was refused after a refused start: %v", err)
		}
	})
}

func meshForBudget(t *testing.T) (*MeshManager, string) {
	t.Helper()
	h, uuid, _ := seedNodeOwned(t)
	return &MeshManager{handler: h, pendingWrites: map[string]*pendingWrite{}}, uuid
}

// L15: a replace that shrinks the server gave noteDiskWrite a negative delta,
// which it dropped, so the gauge kept the old file until the next sweep and
// refused uploads that fit.
func TestAShrinkingWriteRemeasuresTheServer(t *testing.T) {
	_, rdb := gaugeRedis(t)
	root := t.TempDir()
	prevMgr := globalStorageMgr
	t.Cleanup(func() { globalStorageMgr = prevMgr })
	globalStorageMgr = NewStorageManager(root, rdb)
	const uuid = "15151515-1515-1515-1515-151515151515"
	if err := os.MkdirAll(filepath.Join(root, uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, uuid, "world.zip"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rdb.Set(ctx, diskGaugeKey(uuid), `{"total":4000000,"limit":10000000}`, 10*time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})

	noteDiskWrite(ctx, rdb, uuid, -3000000)
	deadline := time.Now().Add(3 * time.Second)
	for gaugeTotal(t, rdb, uuid) == 4000000 {
		if time.Now().After(deadline) {
			t.Fatal("the gauge still holds the replaced file")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
