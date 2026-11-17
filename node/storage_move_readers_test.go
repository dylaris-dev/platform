package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	beampb "dylaris-proto/beam"
	pb "dylaris-proto/node"
)

// blockingBeamStream holds the first chunk until release closes, so the zip
// walk is still reading the tree when the move is tried.
type blockingBeamStream struct {
	fakeBeamDownloadStream
	started chan struct{}
	release chan struct{}
	once    bool
}

func (b *blockingBeamStream) Send(c *beampb.BeamChunk) error {
	if !b.once {
		b.once = true
		close(b.started)
		<-b.release
	}
	return b.fakeBeamDownloadStream.Send(c)
}

// S05: a folder zip download took no hold, so a local storage move copied the
// tree, deleted the source under the running walk, and the walk skipped every
// vanished file: a well-formed zip with files missing, reported complete. A
// move is refused while a folder download runs and goes through once it ended.
func TestAStorageMoveIsRefusedWhileAFolderZipDownloadRuns(t *testing.T) {
	const uuid = "11111111-1111-1111-1111-111111111111"
	for _, tc := range []struct {
		name  string
		start func(t *testing.T, sm *StorageManager, started, release chan struct{})
	}{
		{"panel zip_if_dir", func(t *testing.T, sm *StorageManager, started, release chan struct{}) {
			first := true
			NewStreamHandler(sm).HandleStreaming(&pb.NodeMessage{RequestId: "r", ServerUuid: uuid,
				Payload: &pb.NodeMessage_ReadReq{ReadReq: &pb.ReadFileReq{Path: "survival", ZipIfDir: true}}},
				func(*pb.NodeMessage) error {
					if first {
						first = false
						close(started)
						<-release
					}
					return nil
				})
		}},
		{"panel selective", func(t *testing.T, sm *StorageManager, started, release chan struct{}) {
			first := true
			NewStreamHandler(sm).HandleStreaming(&pb.NodeMessage{RequestId: "r", ServerUuid: uuid,
				Payload: &pb.NodeMessage_SelectiveReadReq{SelectiveReadReq: &pb.SelectiveReadReq{BasePath: "survival", SelectAll: true}}},
				func(*pb.NodeMessage) error {
					if first {
						first = false
						close(started)
						<-release
					}
					return nil
				})
		}},
		{"beam folder", func(t *testing.T, sm *StorageManager, started, release chan struct{}) {
			bs, _, ctx := newTestBeamServer(t)
			bs.storageMgr = sm
			s := &blockingBeamStream{fakeBeamDownloadStream: fakeBeamDownloadStream{ctx: ctx}, started: started, release: release}
			bs.DownloadFile(&beampb.BeamDownloadReq{Path: "survival", ZipIfDir: true}, s)
		}},
		{"beam selective", func(t *testing.T, sm *StorageManager, started, release chan struct{}) {
			bs, _, ctx := newTestBeamServer(t)
			bs.storageMgr = sm
			s := &blockingBeamStream{fakeBeamDownloadStream: fakeBeamDownloadStream{ctx: ctx}, started: started, release: release}
			bs.DownloadSelective(&beampb.BeamSelectiveReq{BasePath: "survival", SelectAll: true}, s)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm, paths := newPlacementManager(t, 2)
			src := filepath.Join(paths[0], uuid, "survival", "world", "level.dat")
			if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(src, []byte("LEVEL"), 0o644); err != nil {
				t.Fatal(err)
			}
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() { defer close(done); tc.start(t, sm, started, release) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("the download never sent")
			}

			if err := sm.CheckMigration(uuid, paths[1]); err == nil {
				t.Error("CheckMigration passed during a folder download")
			}
			if err := sm.MigrateServerPath(uuid, paths[1]); err == nil {
				t.Error("the move ran during a folder download")
			}
			if _, err := os.Stat(src); err != nil {
				t.Fatalf("the source went while the download read it: %v", err)
			}

			close(release)
			<-done
			if err := sm.MigrateServerPath(uuid, paths[1]); err != nil {
				t.Fatalf("the move after the download ended: %v", err)
			}
		})
	}
}
