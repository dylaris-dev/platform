package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"dylaris-pkg/beam/quota"
	"dylaris-pkg/fileperms"

	"github.com/pkg/sftp"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/peer"

	pb "dylaris-proto/beam"
	nodepb "dylaris-proto/node"
)

// fifthPassBeam is a Beam server with full file permissions on uuid, and the
// context its calls carry.
func fifthPassBeam(t *testing.T, sm *StorageManager, rdb *redis.Client, uuid string, port int) (*beamServer, context.Context) {
	t.Helper()
	bs := &beamServer{storageMgr: sm, rdb: rdb, throttle: NewBeamThrottle(context.Background(), nil)}
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
	bs.serverUUIDByPeer.Store(addr.String(), uuid)
	full := fileperms.Full()
	bs.permsByPeer.Store(addr.String(), &full)
	bs.usernameByPeer.Store(addr.String(), "beam-"+uuid)
	if err := os.MkdirAll(sm.GetServerDir(uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	return bs, ctx
}

func forgetUnmeasured(t *testing.T, uuid string) {
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})
}

// K25: a budget kept a private left from the moment it was made, so two
// writes started together each had the whole free space and the whole server
// headroom.
func TestBudgetsStartedTogetherShareTheSpace(t *testing.T) {
	withDiskBudget(t, 100, 1000)
	const uuid = "25252525-2525-2525-2525-252525252525"
	for _, tc := range []struct {
		name     string
		headroom int64 // < 0: no server bound
	}{
		{"node free space", -1},
		{"server headroom", 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := serverDiskHeadroom
			t.Cleanup(func() { serverDiskHeadroom = prev })
			serverDiskHeadroom = func(string) (string, int64, bool) { return uuid, tc.headroom, tc.headroom >= 0 }
			if tc.headroom >= 0 {
				withDiskBudget(t, 1<<30, 1000)
			}
			a, b := newWriteBudget("x"), newWriteBudget("x")
			defer a.release()
			defer b.release()
			if !a.spend(70) {
				t.Fatal("the first write was refused")
			}
			if b.spend(70) {
				t.Error("a budget made at the same time as another wrote past what both shared")
			}
		})
	}
}

// K25: an SFTP handle held its node budget from open, the same way.
func TestSFTPHandlesOpenedTogetherShareTheNodesSpace(t *testing.T) {
	withDiskBudget(t, 100, 1000)
	sm, _ := newPlacementManager(t, 1)
	const uuid = "25252525-2525-2525-2525-252525252526"
	if err := os.MkdirAll(filepath.Join(sm.Paths()[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
	open := func(name string) io.WriterAt {
		w, err := newVirtualFS(refs, sm, nil, "k25").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/" + name, Flags: 0x02 | 0x08 | 0x10})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { w.(io.Closer).Close() })
		return w
	}
	a, b := open("a"), open("b")
	if _, err := a.WriteAt(make([]byte, 70), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.WriteAt(make([]byte, 70), 0); err == nil {
		t.Error("a second handle wrote past the node's free space the first had used")
	}
}

// K26: a handle read the remaining daily quota at open, and a handle that
// closed since moved its bytes out of the only count this one read.
func TestAnOpenSFTPHandleSeesQuotaBookedSince(t *testing.T) {
	rdb, mr := newNodeTestRedis(t)
	mr.Set(quota.DailyUploadBytesKey, "100")
	sm, _ := newPlacementManager(t, 1)
	const uuid = "26262626-2626-2626-2626-262626262626"
	if err := os.MkdirAll(filepath.Join(sm.Paths()[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
	open := func(name string) io.WriterAt {
		w, err := newVirtualFS(refs, sm, rdb, "k26").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/" + name, Flags: 0x02 | 0x08 | 0x10})
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	a, b := open("a"), open("b")
	defer a.(io.Closer).Close()
	if _, err := b.WriteAt(make([]byte, 60), 0); err != nil {
		t.Fatal(err)
	}
	b.(io.Closer).Close()
	if _, err := a.WriteAt(make([]byte, 60), 0); err == nil {
		t.Error("a handle opened earlier spent quota a closed handle had booked")
	}
	if _, err := a.WriteAt(make([]byte, 40), 0); err != nil {
		t.Errorf("what is left was refused: %v", err)
	}
}

// K27: a save checked only the gauge and the booked quota, so it spent what a
// running upload had reserved.
func TestABeamSaveSeesWhatUploadsReserved(t *testing.T) {
	rdb, mr := newNodeTestRedis(t)
	sm := NewStorageManager(t.TempDir(), rdb)
	for _, tc := range []struct {
		name    string
		uuid    string
		reserve func(uuid string) func()
	}{
		{"server disk", "27272727-2727-2727-2727-272727272721", func(uuid string) func() {
			_, release := reserveOn(serverInflightOf(uuid), 60)
			return release
		}},
		{"sftp bytes", "27272727-2727-2727-2727-272727272722", func(uuid string) func() {
			_, release := reserveOn(uploadServerInflight(uuid), 60)
			return release
		}},
		{"daily quota", "27272727-2727-2727-2727-272727272723", func(uuid string) func() {
			mr.Set(quota.DailyUploadBytesKey, "100")
			_, release := reserveOn(uploadAccountInflight("beam-"+uuid), 60)
			return func() { release(); mr.Del(quota.DailyUploadBytesKey) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs, ctx := fifthPassBeam(t, sm, rdb, tc.uuid, 42270)
			forgetUnmeasured(t, tc.uuid)
			rdb.Set(ctx, diskGaugeKey(tc.uuid), `{"total":0,"limit":100}`, 10*time.Minute)
			release := tc.reserve(tc.uuid)
			defer release()
			resp, err := bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "a.txt", Content: string(make([]byte, 60))})
			if err == nil && resp.GetSuccess() {
				t.Error("a save spent what an upload in progress had reserved")
			}
		})
	}
}

// K28: Beam and SFTP deletes (and an SFTP rename over a file) left the gauge
// where it was, so uploads were refused for the space they had just freed
// until the next sweep.
func TestBeamAndSFTPDeletesRemeasureTheServer(t *testing.T) {
	_, rdb := gaugeRedis(t)
	prevSM, prevQ := globalStorageMgr, globalQuotaSet
	t.Cleanup(func() { globalStorageMgr, globalQuotaSet = prevSM, prevQ })
	globalStorageMgr, globalQuotaSet = NewStorageManager(t.TempDir(), rdb), nil
	sm := globalStorageMgr
	for i, del := range []func(uuid string) error{
		func(uuid string) error {
			bs, ctx := fifthPassBeam(t, sm, rdb, uuid, 42280)
			_, err := bs.DeleteFile(ctx, &pb.BeamFileDeleteReq{Path: "world.dat"})
			return err
		},
		func(uuid string) error {
			refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
			return newVirtualFS(refs, sm, rdb, "k28").Filecmd(&sftp.Request{Method: "Remove", Filepath: "s/world.dat"})
		},
		func(uuid string) error {
			refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
			r := &sftp.Request{Method: "Setstat", Filepath: "s/world.dat", Flags: 0x01, Attrs: make([]byte, 8)} // size = 0
			return newVirtualFS(refs, sm, rdb, "k28").Filecmd(r)
		},
		// R12: a rename onto an existing file frees the replaced one.
		func(uuid string) error {
			if err := os.WriteFile(filepath.Join(sm.GetServerDir(uuid), "small.dat"), []byte("x"), 0o644); err != nil {
				return err
			}
			refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
			return newVirtualFS(refs, sm, rdb, "k28").Filecmd(&sftp.Request{Method: "Rename", Filepath: "s/small.dat", Target: "s/world.dat"})
		},
	} {
		uuid := []string{"28282828-2828-2828-2828-282828282821", "28282828-2828-2828-2828-282828282822", "28282828-2828-2828-2828-282828282823", "28282828-2828-2828-2828-282828282824"}[i]
		serverDir := sm.GetServerDir(uuid)
		if err := os.MkdirAll(serverDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(serverDir, "world.dat"), make([]byte, 5000), 0o644); err != nil {
			t.Fatal(err)
		}
		forgetUnmeasured(t, uuid)
		recordDiskLimit(context.Background(), rdb, uuid, 1)
		rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":900000,"limit":1048576}`, time.Minute)
		if err := del(uuid); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for gaugeTotal(t, rdb, uuid) == 900000 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if got := gaugeTotal(t, rdb, uuid); got == 900000 {
			t.Errorf("delete %d left the gauge at %d", i, got)
		}
	}
}

// K29: a delete made every missing parent of its path.
func TestABeamDeleteMakesNoDirectories(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	const uuid = "29292929-2929-2929-2929-292929292929"
	bs, ctx := fifthPassBeam(t, sm, nil, uuid, 42290)
	bs.DeleteFile(ctx, &pb.BeamFileDeleteReq{Path: "a/b/c/missing"})
	if _, err := os.Stat(filepath.Join(sm.GetServerDir(uuid), "a")); err == nil {
		t.Error("a delete of a missing path created its parent directories")
	}
}

// K31: a finished copy took its bytes off the in-flight count and put them
// nowhere an open SFTP handle reads, so the handle wrote the copy's size again.
func TestAnOpenSFTPHandleSeesACopyThatFinishedSince(t *testing.T) {
	_, rdb := gaugeRedis(t)
	sm, _ := newPlacementManager(t, 1)
	const uuid = "31313131-3131-3131-3131-313131313131"
	if err := os.MkdirAll(filepath.Join(sm.Paths()[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	forgetUnmeasured(t, uuid)
	rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":0,"limit":100}`, 10*time.Minute)
	withDiskBudget(t, 1<<30, 1000)
	prev := serverDiskHeadroom
	t.Cleanup(func() { serverDiskHeadroom = prev })
	serverDiskHeadroom = func(string) (string, int64, bool) { return uuid, 100, true }

	refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
	w, err := newVirtualFS(refs, sm, rdb, "k31").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/a", Flags: 0x02 | 0x08 | 0x10})
	if err != nil {
		t.Fatal(err)
	}
	defer w.(io.Closer).Close()
	copied := newWriteBudget("x")
	copied.spend(60)
	copied.release()
	if _, err := w.WriteAt(make([]byte, 60), 0); err == nil {
		t.Error("an open handle wrote past a copy that finished after it opened")
	}
}

// K32: the sweeper judged a temp by its mtime alone, and a slow upload's temp
// was removed under it.
func TestTheSweeperKeepsATempAStreamHolds(t *testing.T) {
	root := t.TempDir()
	sm := NewStorageManager(root, nil)
	held := filepath.Join(root, "srv-uuid", "survival", ".beam-upload-slow")
	if err := os.MkdirAll(filepath.Dir(held), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(held, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(held, old, old); err != nil {
		t.Fatal(err)
	}
	bs := &beamServer{uploadTemps: map[string]*uploadClaim{"srv-uuid/survival/.beam-upload-slow": {refs: 1}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sweepStaleUploadTemps(ctx, sm, bs.uploadTempHeld)
	if _, err := os.Stat(held); err != nil {
		t.Errorf("the sweeper removed a temp a stream still holds: %v", err)
	}
}

// K33: writeEULA truncated through a hard link to the node's config.
func TestTheEULAIsNotWrittenThroughALinkToANodeFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("link counts are read on unix only")
	}
	_, _, root := seedNodeOwned(t)
	cfg := filepath.Join(root, ".node_config.json")
	if err := os.Link(cfg, filepath.Join(root, "survival", "eula.txt")); err != nil {
		t.Fatal(err)
	}
	if err := writeEULA(root, "survival"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfg); string(b) != `{"extraJvmFlags":"-Dsecret=1"}` {
		t.Errorf("the node's config now reads %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "survival", "eula.txt")); string(b) != "eula=true\n" {
		t.Errorf("eula.txt reads %q", b)
	}
}

// K34: a panel copy took no writer hold, so a move fenced while it ran.
// A running move is refused outright now (L14); what is held here is the path
// lookup, so the copy is caught after its hold and before it finishes.
func TestAPanelFileChangeHoldsTheServerAgainstAMove(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	h.storageMgr.mu.Lock()
	release := h.storageMgr.mu.Unlock
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Handle(&nodepb.NodeMessage{RequestId: "c", ServerUuid: uuid, Payload: &nodepb.NodeMessage_CopyReq{CopyReq: &nodepb.CopyFileReq{SrcPath: "survival", DstPath: "creative"}}})
	}()
	writers := func() int {
		h.storageMgr.movingMu.Lock()
		defer h.storageMgr.movingMu.Unlock()
		return h.storageMgr.writers[uuid]
	}
	deadline := time.Now().Add(2 * time.Second)
	for writers() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := writers()
	release()
	<-done
	if got == 0 {
		t.Error("a panel copy ran without a writer hold")
	}
	if n := writers(); n != 0 {
		t.Errorf("a finished copy holds %d writer(s)", n)
	}
}
