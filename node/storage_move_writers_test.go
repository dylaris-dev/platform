package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/fileperms"
	pb "dylaris-proto/node"

	"github.com/alicebob/miniredis/v2"
	"github.com/pkg/sftp"
	"github.com/redis/go-redis/v9"
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

// L14: a create, delete, rename or copy during a storage move took a file
// slot, then waited inside the path lookup for the whole move. Core gave up
// after 30s, and the operation still ran on the new location once the move
// ended - twice after a retry. They are refused at once, as the upload is.
func TestFileOperationsDuringAStorageMoveAreRefusedWithoutWaiting(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	release, err := h.storageMgr.fenceMove(uuid)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ops := []struct {
		name string
		msg  *pb.NodeMessage
	}{
		{"create", &pb.NodeMessage{Payload: &pb.NodeMessage_CreateReq{CreateReq: &pb.CreateFileReq{Path: "survival/new.txt"}}}},
		{"delete", &pb.NodeMessage{Payload: &pb.NodeMessage_DeleteReq{DeleteReq: &pb.DeleteFileReq{Path: "survival/plugins/ok.jar"}}}},
		{"rename", &pb.NodeMessage{Payload: &pb.NodeMessage_RenameReq{RenameReq: &pb.RenameFileReq{OldPath: "survival/plugins/ok.jar", NewName: "b.jar"}}}},
		{"copy", &pb.NodeMessage{Payload: &pb.NodeMessage_CopyReq{CopyReq: &pb.CopyFileReq{SrcPath: "survival/plugins/ok.jar", DstPath: "survival/c.jar"}}}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			op.msg.RequestId, op.msg.ServerUuid = op.name, uuid
			got := make(chan []*pb.NodeMessage, 1)
			go func() { got <- h.Handle(op.msg) }()
			var reply []*pb.NodeMessage
			select {
			case reply = <-got:
			case <-time.After(5 * time.Second):
				t.Fatal("the request waited for the storage move")
			}
			if len(reply) != 1 || reply[0].GetError().GetCode() != 409 {
				t.Fatalf("got %v, want one 409", reply)
			}
			if n := h.storageMgr.writers[uuid]; n != 0 {
				t.Errorf("a refused request holds %d writer(s)", n)
			}
		})
	}
}

// L30: migrate_storage stopped the server before the move's refusals, so a
// move refused for a running upload or backup took the server and its players
// down for nothing. CheckMigration makes those refusals up front and moves
// nothing.
func TestCheckMigrationRefusesWhatTheMoveWouldRefuse(t *testing.T) {
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7"
	for _, tc := range []struct {
		name    string
		create  bool
		writer  bool
		target  int
		refused bool
	}{
		{"movable", true, false, 1, false},
		{"write in progress", true, true, 1, true},
		{"not on any path", false, false, 1, true},
		{"already on the target", true, false, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm, paths := newPlacementManager(t, 2)
			if tc.create {
				if err := os.MkdirAll(filepath.Join(paths[0], uuid), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.writer {
				defer sm.holdWriter(uuid)()
			}
			err := sm.CheckMigration(uuid, paths[tc.target])
			if (err != nil) != tc.refused {
				t.Fatalf("CheckMigration = %v, want refused %v", err, tc.refused)
			}
			if _, err := os.Stat(filepath.Join(paths[1], uuid)); tc.target == 1 && !os.IsNotExist(err) {
				t.Fatalf("the check wrote to the target: %v", err)
			}
		})
	}
}

// fenceOnLookup tries to fence a move of uuid at every storage-path lookup,
// which is the moment a move the read loop would then wait for can start.
type fenceOnLookup struct {
	sm      **StorageManager
	uuid    string
	fenced  int
	lookups int
}

func (f *fenceOnLookup) DialHook(next redis.DialHook) redis.DialHook { return next }
func (f *fenceOnLookup) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" && strings.HasSuffix(fmt.Sprint(cmd.Args()[1]), ":server:"+f.uuid+":storage") {
			f.lookups++
			if release, err := (*f.sm).fenceMove(f.uuid); err == nil {
				f.fenced++
				release()
			}
		}
		return next(ctx, cmd)
	}
}
func (f *fenceOnLookup) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// Q05: a staging file that could not be created released the writer hold
// before handleWrite mapped the error, and handleWrite looks the server path
// up again. A move that fenced in that gap made the read loop wait for the
// whole move. Every lookup of that WriteReq now runs under the hold.
func TestAFailedUploadStagingFileKeepsTheWriterHoldThroughItsReply(t *testing.T) {
	const uuid = "84848484-8484-8484-8484-848484848484"
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	var sm *StorageManager
	hook := &fenceOnLookup{sm: &sm, uuid: uuid}
	rdb.AddHook(hook)
	sm = NewStorageManager(t.TempDir(), rdb)
	h := NewStreamHandler(sm)
	root := sm.GetServerDir(uuid)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// A file where the upload's parent directory should be: the staging
	// create fails, and so does handleWrite's own mkdir.
	if err := os.WriteFile(filepath.Join(root, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	hook.lookups, hook.fenced = 0, 0
	stream := &fakeCoreStream{}
	m := &MeshManager{handler: h, pendingWrites: map[string]*pendingWrite{}}
	m.handleRequest(&coreConnection{stream: stream}, &pb.NodeMessage{RequestId: "w", ServerUuid: uuid, Payload: &pb.NodeMessage_WriteReq{WriteReq: &pb.WriteFileReq{Path: "blocker/a.txt", TotalSize: 3}}})

	if hook.lookups < 2 {
		t.Fatalf("expected the staging create and handleWrite to look the path up, saw %d lookup(s)", hook.lookups)
	}
	if hook.fenced != 0 {
		t.Errorf("a move fenced %d time(s) while the failed WriteReq was still being answered", hook.fenced)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if len(stream.sent) != 1 || stream.sent[0].GetError() == nil {
		t.Fatalf("want one error reply, got %v", stream.sent)
	}
	if n := sm.writers[uuid]; n != 0 {
		t.Errorf("the refused upload still holds %d writer(s)", n)
	}
}
