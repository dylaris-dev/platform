package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"github.com/pkg/sftp"
	"google.golang.org/grpc/peer"
)

// L28: the panel's create, delete and rename take the storage-move writer hold
// before they resolve the server's path; Beam's and SFTP's did not. One that
// resolved the source just before a move fenced then deleted or renamed inside
// the tree the copy was reading, and the move carried the half-done tree over.
// Each must hold the server from before its first path lookup, which is what a
// move that fences first makes wait.
func TestMetadataWritesHoldOffAStorageMove(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   func(sm *StorageManager)
	}{
		{"beam create", func(sm *StorageManager) {
			bs, ctx := movingBeamServer(t, sm)
			bs.CreateFile(ctx, &pb.BeamFileCreateReq{Path: "survival/new", IsDir: true})
		}},
		{"beam delete", func(sm *StorageManager) {
			bs, ctx := movingBeamServer(t, sm)
			bs.DeleteFile(ctx, &pb.BeamFileDeleteReq{Path: "survival/a.txt"})
		}},
		{"beam rename", func(sm *StorageManager) {
			bs, ctx := movingBeamServer(t, sm)
			bs.RenameFile(ctx, &pb.BeamFileRenameReq{OldPath: "survival/a.txt", NewName: "b.txt"})
		}},
		{"sftp mkdir", func(sm *StorageManager) {
			movingSFTP(sm).Filecmd(&sftp.Request{Method: "Mkdir", Filepath: "s/survival/new"})
		}},
		{"sftp remove", func(sm *StorageManager) {
			movingSFTP(sm).Filecmd(&sftp.Request{Method: "Remove", Filepath: "s/survival/a.txt"})
		}},
		{"sftp rename", func(sm *StorageManager) {
			movingSFTP(sm).Filecmd(&sftp.Request{Method: "Rename", Filepath: "s/survival/a.txt", Target: "s/survival/b.txt"})
		}},
		{"sftp setstat", func(sm *StorageManager) {
			movingSFTP(sm).Filecmd(&sftp.Request{Method: "Setstat", Filepath: "s/survival/a.txt", Flags: 0x01, Attrs: make([]byte, 8)})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm, paths := newMovableServer(t)
			if err := os.WriteFile(filepath.Join(paths[0], movingUUID, "survival", "a.txt"), []byte("hi"), 0o644); err != nil {
				t.Fatal(err)
			}
			copying, release := make(chan struct{}), make(chan struct{})
			prev := migrateCopyTree
			t.Cleanup(func() { migrateCopyTree = prev })
			migrateCopyTree = func(src, dst string) error {
				close(copying)
				<-release
				return prev(src, dst)
			}
			moved := make(chan error, 1)
			go func() { moved <- sm.MigrateServerPath(movingUUID, paths[1]) }()
			<-copying

			done := make(chan struct{})
			go func() { tc.op(sm); close(done) }()
			held := false
			for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && !held; time.Sleep(5 * time.Millisecond) {
				sm.movingMu.Lock()
				held = sm.writers[movingUUID] > 0
				sm.movingMu.Unlock()
			}
			close(release)
			if err := <-moved; err != nil {
				t.Fatalf("move: %v", err)
			}
			<-done
			if !held {
				t.Fatal("the operation resolved the server's path without holding it")
			}
		})
	}
}

func movingBeamServer(t *testing.T, sm *StorageManager) (*beamServer, context.Context) {
	bs, _, ctx := newTestBeamServer(t)
	bs.storageMgr = sm
	p, _ := peer.FromContext(ctx)
	bs.serverUUIDByPeer.Store(p.Addr.String(), movingUUID)
	return bs, ctx
}

func movingSFTP(sm *StorageManager) *virtualFS {
	return newVirtualFS([]sftpServerRef{{UUID: movingUUID, Name: "s", Perms: fileperms.Full()}}, sm, nil, "alice")
}
