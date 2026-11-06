package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/fileperms"
	pb "dylaris-proto/node"

	"github.com/pkg/sftp"
)

// H40: the move fence only holds back lookups made after it. An SFTP handle
// opened before it kept writing into the source tree, which the move then
// deleted, and the client saw a successful upload of a truncated file. A move
// is refused while a handle is open for writing and goes through once it
// closed.
func TestAStorageMoveWaitsForNoOpenSFTPWriter(t *testing.T) {
	sm, paths := newPlacementManager(t, 2)
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7"
	if err := os.MkdirAll(filepath.Join(paths[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
	w, err := newVirtualFS(refs, sm, nil, "alice").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/world.zip", Flags: 0x02 | 0x08 | 0x10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt([]byte("half"), 0); err != nil {
		t.Fatal(err)
	}

	if err := sm.MigrateServerPath(uuid, paths[1]); err == nil {
		t.Fatal("the move ran under an open SFTP write handle")
	}
	if _, err := os.Stat(filepath.Join(paths[0], uuid, "world.zip")); err != nil {
		t.Fatalf("the refused move touched the source: %v", err)
	}

	if err := w.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	if err := sm.MigrateServerPath(uuid, paths[1]); err != nil {
		t.Fatalf("the move after the handle closed: %v", err)
	}
}

// K10/K36: a WriteReq runs on the shared Core read loop. During a storage move
// its path lookup waited on the move fence for the whole copy, so the node
// read nothing else from that Core. It is refused at once now, and a write
// that got in first still makes the move wait (holdWriter above).
func TestAnUploadDuringAStorageMoveIsRefusedWithoutBlocking(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	release, err := h.storageMgr.fenceMove(uuid)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stream := &fakeCoreStream{}
	m := &MeshManager{handler: h, pendingWrites: map[string]*pendingWrite{}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.handleRequest(&coreConnection{stream: stream}, &pb.NodeMessage{RequestId: "w", ServerUuid: uuid, Payload: &pb.NodeMessage_WriteReq{WriteReq: &pb.WriteFileReq{Path: "survival/a.txt", TotalSize: 3}}})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		release()
		<-done
		t.Fatal("the WriteReq blocked the read loop on the storage move")
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if len(stream.sent) != 1 || stream.sent[0].GetError().GetCode() != 409 {
		t.Fatalf("want one 409 reply, got %v", stream.sent)
	}
	if len(m.pendingWrites) != 0 {
		t.Error("a refused upload is still pending")
	}
	if n := h.storageMgr.writers[uuid]; n != 0 {
		t.Errorf("a refused upload holds %d writer(s)", n)
	}
}
