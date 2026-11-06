package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/beam/quota"
	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"
	nodepb "dylaris-proto/node"

	"github.com/alicebob/miniredis/v2"
	"github.com/pkg/sftp"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/peer"
)

const movingUUID = "29292929-2929-2929-2929-292929292929"

func newMovableServer(t *testing.T) (*StorageManager, []string) {
	t.Helper()
	sm, paths := newPlacementManager(t, 2)
	if err := os.MkdirAll(filepath.Join(paths[0], movingUUID, "survival"), 0o755); err != nil {
		t.Fatal(err)
	}
	return sm, paths
}

// J29/J37: only SFTP held the move off. A Beam upload that resolved the server
// before the move finished into the source, which the move then deleted, and
// the client was told "uploaded".
func TestABeamUploadHoldsOffAStorageMove(t *testing.T) {
	bs, _, ctx := newTestBeamServer(t)
	sm, paths := newMovableServer(t)
	bs.storageMgr = sm
	p, _ := peer.FromContext(ctx)
	bs.serverUUIDByPeer.Store(p.Addr.String(), movingUUID)

	var moveErr error
	stream := &hookedUploadStream{
		fakeBeamUploadStream: fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "w.zip", 8, []byte("abcd"), []byte("efgh"))},
		before: func(i int) {
			if i == 2 {
				moveErr = sm.MigrateServerPath(movingUUID, paths[1])
			}
		},
	}
	if err := bs.UploadFile(stream); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if moveErr == nil {
		t.Fatal("the move ran under a Beam upload in progress")
	}
	if err := sm.MigrateServerPath(movingUUID, paths[1]); err != nil {
		t.Fatalf("the move after the upload ended: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths[1], movingUUID, "survival", "w.zip")); err != nil {
		t.Fatalf("the uploaded file did not move with the server: %v", err)
	}
}

// J29/J37: a panel upload over the mesh holds the server until it is done.
func TestAMeshUploadHoldsOffAStorageMove(t *testing.T) {
	sm, paths := newMovableServer(t)
	withDiskBudget(t, 1<<30, 1000)
	h := NewStreamHandler(sm)
	cc := &coreConnection{stream: &fakeCoreStream{}}
	m := &MeshManager{handler: h, pendingWrites: map[string]*pendingWrite{}}

	m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", ServerUuid: movingUUID, Payload: &nodepb.NodeMessage_WriteReq{WriteReq: &nodepb.WriteFileReq{Path: "survival/up.bin", TotalSize: 4}}})
	if _, ok := m.pendingWrites["w"]; !ok {
		t.Fatal("the upload was not accepted")
	}
	if err := sm.MigrateServerPath(movingUUID, paths[1]); err == nil {
		t.Fatal("the move ran under a pending panel upload")
	}
	m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", Payload: &nodepb.NodeMessage_Chunk{Chunk: &nodepb.DataChunk{Data: []byte("abcd")}}})
	m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", Payload: &nodepb.NodeMessage_TransferDone{TransferDone: &nodepb.TransferDone{TotalBytes: 4}}})
	if err := sm.MigrateServerPath(movingUUID, paths[1]); err != nil {
		t.Fatalf("the move after the upload ended: %v", err)
	}
}

// J29/J37: a restore, an install or a version change resolved the server once
// and wrote for minutes; migrate_storage checked none of them.
func TestQueuedWritersHoldOffAStorageMove(t *testing.T) {
	for _, tc := range []struct {
		action  string
		refused bool
	}{
		{"backup_restore", true},
		{"backup_run", true},
		{"install_mod", true},
		{"remove_mod", true},
		{"update_server_version", true},
		{"copy_sub_server", true},
		{"setup", true},
		{"reinstall", true},
		{"start", false},
		{"migrate_storage", false},
	} {
		t.Run(tc.action, func(t *testing.T) {
			sm, paths := newMovableServer(t)
			cmd := NodeCommand{Action: tc.action, ServerUUID: movingUUID}
			release := holdCommandWriter(sm, cmd)
			err := sm.MigrateServerPath(movingUUID, paths[1])
			release()
			if refused := err != nil; refused != tc.refused {
				t.Fatalf("move refused = %v (%v), want %v", refused, err, tc.refused)
			}
		})
	}
}

// J35: SFTP took no part in the node's free-space reserve, so one upload to a
// server without a limit filled the disk for every server on the node.
func TestAnSFTPUploadStopsAtTheNodesFreeSpace(t *testing.T) {
	withDiskBudget(t, 8, 1000)
	sm, _ := newMovableServer(t)
	refs := []sftpServerRef{{UUID: movingUUID, Name: "s", Perms: fileperms.Full()}}
	w, err := newVirtualFS(refs, sm, nil, "erin").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/survival/big.bin", Flags: 0x02 | 0x08 | 0x10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt(make([]byte, 4), 0); err != nil {
		t.Fatalf("a write inside the free space: %v", err)
	}
	if n := inflightWrites.Load(); n != 4 {
		t.Errorf("the open handle holds %d bytes of the reserve, want 4", n)
	}
	if _, err := w.WriteAt(make([]byte, 8), 4); err == nil {
		t.Error("a write past the node's free space was taken")
	}
	if err := w.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	if n := inflightWrites.Load(); n != 0 {
		t.Errorf("%d bytes still held after close", n)
	}
}

// J35: SaveFileContent did not check the node's free space either.
func TestABeamSaveStopsAtTheNodesFreeSpace(t *testing.T) {
	withDiskBudget(t, 4, 1000)
	bs, uuid, ctx := newTestBeamServer(t)
	if err := os.MkdirAll(filepath.Join(bs.storageMgr.GetServerDir(uuid), "survival"), 0o755); err != nil {
		t.Fatal(err)
	}
	resp, err := bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "survival/a.txt", Content: "12345678"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Success {
		t.Error("a save past the node's free space was written")
	}
	if n := inflightWrites.Load(); n != 0 {
		t.Errorf("%d bytes still held after the save", n)
	}
}

// J30: a truncating overwrite was held against headroom that still counted the
// old file, so a re-upload of a file near the limit failed half-way, after the
// truncate had already destroyed the original.
func TestATruncatingSFTPOverwriteIsCreditedTheOldFile(t *testing.T) {
	_, rdb := gaugeRedis(t)
	sm, paths := newMovableServer(t)
	target := filepath.Join(paths[0], movingUUID, "survival", "p.jar")
	if err := os.WriteFile(target, make([]byte, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	rdb.Set(context.Background(), diskGaugeKey(movingUUID), `{"total":95,"limit":100}`, 10*time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, movingUUID)
		delete(unmeasured.last, movingUUID)
		unmeasured.Unlock()
	})
	refs := []sftpServerRef{{UUID: movingUUID, Name: "s", Perms: fileperms.Full()}}
	w, err := newVirtualFS(refs, sm, rdb, "frank").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/survival/p.jar", Flags: 0x02 | 0x08 | 0x10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt(make([]byte, 50), 0); err != nil {
		t.Errorf("overwriting a file with one of the same size: %v", err)
	}
	if _, err := w.WriteAt(make([]byte, 10), 50); err == nil {
		t.Error("growth past the headroom was taken")
	}
	w.(io.Closer).Close()
}

// J31: Beam and SFTP each counted their own in-flight bytes against the daily
// quota, so an upload over each at once had the whole allowance twice.
func TestBeamAndSFTPShareTheDailyQuotaInFlight(t *testing.T) {
	t.Run("an SFTP upload in progress is seen by a Beam upload", func(t *testing.T) {
		bs, _, ctx := newTestBeamServer(t)
		mr := miniredis.RunT(t)
		bs.rdb = redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { bs.rdb.Close() })
		mr.Set(quota.DailyUploadBytesKey, "100")
		p, _ := peer.FromContext(ctx)
		bs.usernameByPeer.Store(p.Addr.String(), "gina")

		_, release := reserveOn(uploadAccountInflight("gina"), 60)
		defer release()
		stream := &fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "w.zip", 60)}
		if err := bs.UploadFile(stream); err == nil {
			t.Fatal("a Beam upload passed the quota an SFTP upload is already using")
		}
	})
	t.Run("a Beam upload in progress is seen by an SFTP upload", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { rdb.Close() })
		mr.Set(quota.DailyUploadBytesKey, "100")
		sm, _ := newMovableServer(t)

		_, release := reserveOn(uploadAccountInflight("hank"), 60)
		defer release()
		refs := []sftpServerRef{{UUID: movingUUID, Name: "s", Perms: fileperms.Full()}}
		w, err := newVirtualFS(refs, sm, rdb, "hank").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/survival/w.zip", Flags: 0x02 | 0x08 | 0x10})
		if err != nil {
			t.Fatal(err)
		}
		defer w.(io.Closer).Close()
		if _, err := w.WriteAt(make([]byte, 60), 0); err == nil {
			t.Fatal("an SFTP upload passed the quota a Beam upload is already using")
		}
	})
}

// J33/J36: a Redis error read as "no throttle" and lifted an active per-server
// data throttle until the next successful read.
func TestARedisErrorKeepsTheServerDataThrottle(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	const uuid = "33333333-3333-3333-3333-333333333333"
	mr.Set("dylaris:server:"+uuid+":data_bw", "1000")
	th := NewServerDataThrottle(rdb)
	ctx := context.Background()
	if th.limiter(ctx, uuid) == nil {
		t.Fatal("no limiter for a throttled server")
	}

	th.m[uuid].at = time.Now().Add(-time.Minute)
	mr.SetError("LOADING Redis is loading the dataset in memory")
	if th.limiter(ctx, uuid) == nil {
		t.Fatal("a Redis error lifted the throttle")
	}

	mr.SetError("")
	mr.Del("dylaris:server:" + uuid + ":data_bw")
	th.m[uuid].at = time.Now().Add(-time.Minute)
	if th.limiter(ctx, uuid) != nil {
		t.Fatal("a removed throttle is still applied")
	}
}
