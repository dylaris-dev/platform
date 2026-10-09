package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/fileperms"

	"github.com/alicebob/miniredis/v2"
	"github.com/pkg/sftp"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/peer"

	pb "dylaris-proto/beam"
	nodepb "dylaris-proto/node"
)

func gaugeRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return mr, rdb
}

func gaugeTotal(t *testing.T, rdb *redis.Client, uuid string) int64 {
	t.Helper()
	total, _ := serverDiskGauge(context.Background(), rdb, uuid)
	return total
}

// The gauge is a measurement minutes apart: a file closed after it was
// invisible to the next check, so upload, close, upload again each saw the
// whole headroom.
func TestSequentialSFTPUploadsSeeEachOther(t *testing.T) {
	_, rdb := gaugeRedis(t)
	sm, _ := newPlacementManager(t, 1)
	const uuid = "92929292-9292-9292-9292-929292929292"
	if err := os.MkdirAll(filepath.Join(sm.Paths()[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":0,"limit":100}`, 10*time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})
	refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
	upload := func(name string, n int) error {
		w, err := newVirtualFS(refs, sm, rdb, "alice").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/" + name, Flags: 0x02 | 0x08 | 0x10})
		if err != nil {
			t.Fatal(err)
		}
		defer w.(io.Closer).Close()
		_, err = w.WriteAt(make([]byte, n), 0)
		return err
	}
	if err := upload("a", 60); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if got := gaugeTotal(t, rdb, uuid); got != 60 {
		t.Errorf("gauge after the first upload = %d, want 60", got)
	}
	if err := upload("b", 60); err == nil {
		t.Error("a second upload after the first closed saw the whole headroom again")
	}
	if err := upload("c", 40); err != nil {
		t.Errorf("what is left was refused: %v", err)
	}
}

func TestAMeasurementReplacesOnlyTheWritesItSaw(t *testing.T) {
	_, rdb := gaugeRedis(t)
	ctx := context.Background()
	const uuid = "u1"
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})

	before := time.Now()
	publishDiskUsage(ctx, rdb, uuid, &DiskUsagePayload{Total: 10, Limit: 100}, before, time.Minute)
	noteDiskWrite(ctx, rdb, uuid, 30)
	if got := gaugeTotal(t, rdb, uuid); got != 40 {
		t.Fatalf("after a write = %d, want 40", got)
	}
	// A measurement that started before the write cannot have seen it.
	publishDiskUsage(ctx, rdb, uuid, &DiskUsagePayload{Total: 12, Limit: 100}, before, time.Minute)
	if got := gaugeTotal(t, rdb, uuid); got != 42 {
		t.Errorf("measurement from before the write = %d, want 42", got)
	}
	// One that began earlier still, published late, is not the latest.
	publishDiskUsage(ctx, rdb, uuid, &DiskUsagePayload{Total: 1, Limit: 100}, before.Add(-time.Second), time.Minute)
	if got := gaugeTotal(t, rdb, uuid); got != 42 {
		t.Errorf("an older measurement published late replaced a newer one: %d", got)
	}
	// One that started after it holds it already.
	time.Sleep(10 * time.Millisecond)
	publishDiskUsage(ctx, rdb, uuid, &DiskUsagePayload{Total: 40, Limit: 100}, time.Now(), time.Minute)
	if got := gaugeTotal(t, rdb, uuid); got != 40 {
		t.Errorf("newer measurement = %d, want 40", got)
	}
	unmeasured.Lock()
	left := len(unmeasured.m[uuid])
	unmeasured.Unlock()
	if left != 0 {
		t.Errorf("%d writes kept after a measurement that saw them", left)
	}
}

func TestANoteKeepsTheGaugesExpiry(t *testing.T) {
	mr, rdb := gaugeRedis(t)
	ctx := context.Background()
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, "u2")
		delete(unmeasured.m, "u3")
		delete(unmeasured.m, "u4")
		delete(unmeasured.last, "u2")
		unmeasured.Unlock()
	})

	rdb.Set(ctx, diskGaugeKey("u2"), `{"total":5,"limit":100,"subServers":{"a":5},"enforceable":false}`, 10*time.Minute)
	noteDiskWrite(ctx, rdb, "u2", 7)
	if ttl := mr.TTL(diskGaugeKey("u2")); ttl <= 0 || ttl > 10*time.Minute {
		t.Errorf("ttl = %v", ttl)
	}
	var u DiskUsagePayload
	raw, _ := rdb.Get(ctx, diskGaugeKey("u2")).Result()
	if json.Unmarshal([]byte(raw), &u) != nil || u.Total != 12 || u.SubServers["a"] != 5 {
		t.Errorf("gauge = %s", raw)
	}
	// A gauge with no expiry is not one the node published; a note never
	// writes one that would outlive its server.
	rdb.Set(ctx, diskGaugeKey("u4"), `{"total":5,"limit":100}`, 0)
	noteDiskWrite(ctx, rdb, "u4", 7)
	if got := gaugeTotal(t, rdb, "u4"); got != 5 {
		t.Errorf("a gauge without expiry was rewritten: %d", got)
	}
	// No gauge: nothing to correct, and none is invented with no expiry.
	noteDiskWrite(ctx, rdb, "u3", 7)
	if mr.Exists(diskGaugeKey("u3")) {
		t.Error("a note created a gauge")
	}
}

// A copy or install writes and removes (a wipe, temporary archives), so it
// publishes a fresh measurement instead of adding what it wrote.
func TestACopyRemeasuresTheServer(t *testing.T) {
	_, rdb := gaugeRedis(t)
	prevSM, prevQ := globalStorageMgr, globalQuotaSet
	t.Cleanup(func() { globalStorageMgr, globalQuotaSet = prevSM, prevQ })
	globalStorageMgr, globalQuotaSet = NewStorageManager(t.TempDir(), rdb), nil
	withDiskBudget(t, 1<<30, 1000)
	const uuid = "92929292-9292-9292-9292-929292929294"
	serverDir := globalStorageMgr.GetServerDir(uuid)
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "world.dat"), make([]byte, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	recordDiskLimit(context.Background(), rdb, uuid, 1)
	// The gauge still holds what a wipe has since removed.
	rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":900000,"limit":1048576}`, time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})

	b := newWriteBudget(serverDir)
	b.spend(5000)
	b.release()
	if got, want := gaugeTotal(t, rdb, uuid), dirSize(serverDir); got != want {
		t.Errorf("gauge after a copy = %d, want the measured %d", got, want)
	}
}

// Overwriting a file frees the old one: re-uploading the same file filled the
// gauge by its whole size each time.
func TestAnOverwriteAddsOnlyTheGrowth(t *testing.T) {
	_, rdb := gaugeRedis(t)
	sm, _ := newPlacementManager(t, 1)
	const uuid = "92929292-9292-9292-9292-929292929295"
	if err := os.MkdirAll(filepath.Join(sm.Paths()[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":0,"limit":1000}`, 10*time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})
	refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
	upload := func(n int) {
		w, err := newVirtualFS(refs, sm, rdb, "alice").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/a", Flags: 0x02 | 0x08 | 0x10})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.WriteAt(make([]byte, n), 0); err != nil {
			t.Fatal(err)
		}
		w.(io.Closer).Close()
	}
	upload(300)
	upload(300)
	upload(350)
	if got := gaugeTotal(t, rdb, uuid); got != 350 {
		t.Errorf("gauge after overwriting = %d, want 350", got)
	}
}

// A handle opened before another upload closed read its ceiling without it.
func TestAnOpenHandleSeesUploadsClosedSince(t *testing.T) {
	_, rdb := gaugeRedis(t)
	sm, _ := newPlacementManager(t, 1)
	const uuid = "92929292-9292-9292-9292-929292929296"
	if err := os.MkdirAll(filepath.Join(sm.Paths()[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":0,"limit":100}`, 10*time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})
	refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
	open := func(name string) io.WriterAt {
		w, err := newVirtualFS(refs, sm, rdb, "alice").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/" + name, Flags: 0x02 | 0x08 | 0x10})
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	a := open("a")
	defer a.(io.Closer).Close()
	b := open("b")
	if _, err := b.WriteAt(make([]byte, 60), 0); err != nil {
		t.Fatal(err)
	}
	b.(io.Closer).Close()
	if _, err := a.WriteAt(make([]byte, 60), 0); err == nil {
		t.Error("a handle opened earlier ignored an upload that closed since")
	}
	if _, err := a.WriteAt(make([]byte, 40), 0); err != nil {
		t.Errorf("what is left was refused: %v", err)
	}
}

// Beam checks the same gauge before a save.
func TestSequentialBeamSavesSeeEachOther(t *testing.T) {
	_, rdb := gaugeRedis(t)
	sm := NewStorageManager(t.TempDir(), rdb)
	bs := &beamServer{storageMgr: sm, rdb: rdb, throttle: NewBeamThrottle(context.Background(), nil)}
	const uuid = "92929292-9292-9292-9292-929292929293"
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 42092}
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
	bs.serverUUIDByPeer.Store(addr.String(), uuid)
	full := fileperms.Full()
	bs.permsByPeer.Store(addr.String(), &full)
	if err := os.MkdirAll(sm.GetServerDir(uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	rdb.Set(ctx, diskGaugeKey(uuid), `{"total":0,"limit":100}`, 10*time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})

	save := func(name string, n int) bool {
		resp, err := bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: name, Content: string(make([]byte, n))})
		return err == nil && resp.GetSuccess()
	}
	if !save("a.txt", 60) {
		t.Fatal("the first save was refused")
	}
	if save("b.txt", 60) {
		t.Error("a second save saw the whole headroom again")
	}
}

// The panel's upload reaches the node over the mesh, and Core checks the same
// gauge before the next one.
func TestAPanelUploadIsAddedToTheGauge(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	_, rdb := gaugeRedis(t)
	rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":0,"limit":1000}`, 10*time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})
	cc := &coreConnection{stream: &fakeCoreStream{}}
	m := &MeshManager{handler: h, rdb: rdb, pendingWrites: map[string]*pendingWrite{}}

	upload := func(id string) {
		m.handleRequest(cc, &nodepb.NodeMessage{RequestId: id, ServerUuid: uuid, Payload: &nodepb.NodeMessage_WriteReq{WriteReq: &nodepb.WriteFileReq{Path: "survival/up.bin", TotalSize: 300}}})
		m.handleRequest(cc, &nodepb.NodeMessage{RequestId: id, Payload: &nodepb.NodeMessage_Chunk{Chunk: &nodepb.DataChunk{Data: make([]byte, 300)}}})
		m.handleRequest(cc, &nodepb.NodeMessage{RequestId: id, Payload: &nodepb.NodeMessage_TransferDone{TransferDone: &nodepb.TransferDone{TotalBytes: 300}}})
	}
	upload("w1")
	if got := gaugeTotal(t, rdb, uuid); got != 300 {
		t.Errorf("gauge after a panel upload = %d, want 300", got)
	}
	upload("w2")
	if got := gaugeTotal(t, rdb, uuid); got != 300 {
		t.Errorf("gauge after uploading the same file again = %d, want 300", got)
	}
}
