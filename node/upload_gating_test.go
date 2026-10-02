package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/beam/quota"
	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/peer"
)

func newNodeTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

func TestServerDiskGauge(t *testing.T) {
	rdb, mr := newNodeTestRedis(t)
	ctx := context.Background()

	if total, limit := serverDiskGauge(ctx, nil, "srv-1"); total != 0 || limit != 0 {
		t.Errorf("nil rdb: (%d, %d), want (0, 0)", total, limit)
	}
	if total, limit := serverDiskGauge(ctx, rdb, "srv-1"); total != 0 || limit != 0 {
		t.Errorf("missing gauge: (%d, %d), want (0, 0)", total, limit)
	}
	mr.Set("dylaris:server:srv-1:stats:disk", `{"total":900,"limit":1000}`)
	if total, limit := serverDiskGauge(ctx, rdb, "srv-1"); total != 900 || limit != 1000 {
		t.Errorf("gauge set: (%d, %d), want (900, 1000)", total, limit)
	}
}

// TestSaveFileContent_EnforcesUploadLimits pins that a direct content save obeys
// the same caps as an upload: an over-cap save is refused (Success=false) and a
// successful save is counted against the shared daily quota.
func TestSaveFileContent_EnforcesUploadLimits(t *testing.T) {
	rdb, mr := newNodeTestRedis(t)
	sm := NewStorageManager(t.TempDir(), nil)
	bs := &beamServer{storageMgr: sm, rdb: rdb}

	const uuid = "11111111-1111-1111-1111-111111111111"
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41000}
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
	bs.serverUUIDByPeer.Store(addr.String(), uuid)
	bs.usernameByPeer.Store(addr.String(), "u")
	full := fileperms.Full()
	bs.permsByPeer.Store(addr.String(), &full)
	if err := os.MkdirAll(filepath.Join(sm.GetServerDir(uuid), "survival"), 0755); err != nil {
		t.Fatal(err)
	}

	// Over the size cap -> refused, nothing written.
	mr.Set(quota.MaxUploadBytesKey, "10")
	resp, err := bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "survival/big.txt", Content: strings.Repeat("x", 100)})
	if err != nil {
		t.Fatalf("SaveFileContent: %v", err)
	}
	if resp.Success {
		t.Fatalf("expected refusal over the size cap, got success")
	}

	// Under the cap -> saved and counted (5 bytes).
	mr.Del(quota.MaxUploadBytesKey)
	resp, err = bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "survival/ok.txt", Content: "hello"})
	if err != nil {
		t.Fatalf("SaveFileContent: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success, got: %s", resp.Message)
	}
	if got, _ := rdb.Get(context.Background(), quota.DailyKey("u", time.Now())).Int64(); got != 5 {
		t.Errorf("daily counter = %d, want 5", got)
	}
}

// TestSFTPWriteLimits pins the three limits apart: a size cap per file, the
// disk headroom per server, the daily quota per account.
func TestSFTPWriteLimits(t *testing.T) {
	rdb, mr := newNodeTestRedis(t)
	ctx := context.Background()
	const uuid = "srv-1"

	if lim := sftpWriteLimits(ctx, nil, uuid, "u"); lim.file != nil || lim.disk != -1 || lim.daily != nil {
		t.Errorf("nil rdb = %+v, want no limits", lim)
	}
	mr.Set(quota.MaxUploadBytesKey, "500")
	mr.Set("dylaris:server:"+uuid+":stats:disk", `{"total":900,"limit":1000}`)
	mr.Set(quota.DailyUploadBytesKey, "1000")
	mr.Set(quota.DailyKey("u", time.Now()), "950")
	lim := sftpWriteLimits(ctx, rdb, uuid, "u")
	if lim.file == nil || *lim.file != 500 || lim.disk != 100 || lim.daily == nil || *lim.daily != 50 {
		t.Errorf("limits = file %v disk %d daily %v, want 500/100/50", lim.file, lim.disk, lim.daily)
	}
}

// TestMeteredSFTPWriter pins per-write ceiling enforcement and that the resulting
// file size is recorded against the daily quota on close.
func TestMeteredSFTPWriter(t *testing.T) {
	rdb, _ := newNodeTestRedis(t)
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "f"), os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		t.Fatal(err)
	}
	m := &meteredSFTPWriter{f: f, ceil: 100, reason: "server disk limit", rdb: rdb, username: "u"}

	if _, err := m.WriteAt(make([]byte, 60), 0); err != nil {
		t.Fatalf("write within ceiling: %v", err)
	}
	// end offset 60+50 = 110 > 100 -> refused, not written.
	if _, err := m.WriteAt(make([]byte, 50), 60); err == nil {
		t.Fatalf("expected refusal past the ceiling")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, _ := rdb.Get(context.Background(), quota.DailyKey("u", time.Now())).Int64(); got != 60 {
		t.Errorf("recorded daily bytes = %d, want 60 (the resulting file size)", got)
	}
}
