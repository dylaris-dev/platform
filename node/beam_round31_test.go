package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	beamauth "dylaris-pkg/beam/auth"
	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"dylaris-pkg/beam/quota"
	"dylaris-pkg/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/peer"
)

// An open Beam session kept the rights of its ticket for as long as the
// connection lived, and the client's health pings kept it alive: a removed
// member, a reset password or a suspended account went on working. The stamp
// Core sets now ends the open session at its next operation.
func TestAnOpenBeamSessionEndsWhenAccessChanges(t *testing.T) {
	prev := beamRecheckEvery
	beamRecheckEvery = 0
	t.Cleanup(func() { beamRecheckEvery = prev })

	const secret = "test-beam-secret"
	const srv = "22222222-2222-2222-2222-222222222222"
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40300}})

	full := fileperms.Full()
	tok, err := beamauth.SignBeamTicket(secret, beamauth.BeamClaims{ServerUUID: srv, Username: "carol", Perms: &full})
	if err != nil {
		t.Fatal(err)
	}
	bs := &beamServer{jwtSecret: secret, rdb: rdb}
	if resp, err := bs.Authenticate(ctx, &pb.BeamAuthReq{Ticket: tok}); err != nil || !resp.Ok {
		t.Fatalf("Authenticate: %v %v", resp, err)
	}
	if err := bs.requireFilePerm(ctx, canWrite, "write"); err != nil {
		t.Fatalf("a fresh session was refused: %v", err)
	}

	time.Sleep(1100 * time.Millisecond) // the claim carries whole seconds
	if err := beamauth.BumpAccessEpoch(context.Background(), rdb, srv); err != nil {
		t.Fatal(err)
	}
	if err := bs.requireFilePerm(ctx, canWrite, "write"); err == nil {
		t.Fatal("the open session kept working after its access changed")
	}
	if bs.extractServerUUID(ctx) != "" {
		t.Fatal("the session's server binding survived")
	}
}

// The daily quota was checked per upload against what was already booked, and
// an upload books when it finishes: uploads started together each had the
// whole remaining allowance.
func TestParallelBeamUploadsShareTheDailyQuota(t *testing.T) {
	bs, _, ctx := newTestBeamServer(t)
	mr := miniredis.RunT(t)
	bs.rdb = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { bs.rdb.Close() })
	mr.Set(quota.DailyUploadBytesKey, "100")
	p, _ := peer.FromContext(ctx)
	bs.usernameByPeer.Store(p.Addr.String(), "carol")

	_, release := bs.reserveUpload("u:carol", 60) // another of carol's uploads, still running
	defer release()
	stream := &fakeBeamUploadStream{ctx: ctx, msgs: []*pb.BeamUploadMsg{
		{Payload: &pb.BeamUploadMsg_Start{Start: &pb.BeamUploadStart{Path: "survival", Filename: "w.zip", TotalSize: 60}}},
	}}
	if err := bs.UploadFile(stream); err == nil {
		t.Fatal("a second upload passed the quota the first one is already using")
	}
}

// Reading a file held it all in memory several times over; a world archive read
// in parallel could take the node agent down for every tenant.
func TestBeamWillNotOpenAHugeFileAsText(t *testing.T) {
	bs, uuid, ctx := newTestBeamServer(t)
	dir := bs.storageMgr.GetServerDir(uuid)
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "big.log"), make([]byte, maxBeamReadContent+1), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := bs.ReadFileContent(ctx, &pb.BeamFileReadReq{Path: "big.log"})
	if err != nil || resp.Success || resp.Content != "" {
		t.Fatalf("a file over the limit was returned: success=%v err=%v", resp.GetSuccess(), err)
	}
}

// Beam is the file path in production, and its writes reached no audit trail.
func TestABeamChangeIsSentToTheAuditTrail(t *testing.T) {
	bs, uuid, ctx := newTestBeamServer(t)
	mr := miniredis.RunT(t)
	bs.rdb = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { bs.rdb.Close() })
	prev := nodeID
	nodeID = "node-a"
	t.Cleanup(func() { nodeID = prev })
	p, _ := peer.FromContext(ctx)
	bs.usernameByPeer.Store(p.Addr.String(), "carol")
	dir := bs.storageMgr.GetServerDir(uuid)
	os.MkdirAll(filepath.Join(dir, "survival"), 0o755)
	os.WriteFile(filepath.Join(dir, "survival", "old.jar"), []byte("x"), 0o644)

	sub := bs.rdb.Subscribe(context.Background(), queue.SFTPAuditChannel("node-a"))
	defer sub.Close()
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resp, _ := bs.DeleteFile(ctx, &pb.BeamFileDeleteReq{Path: "survival/old.jar"}); !resp.GetSuccess() {
		t.Fatalf("delete: %+v", resp)
	}
	msg, err := sub.ReceiveMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var rec queue.SFTPAuditRecord
	json.Unmarshal([]byte(msg.Payload), &rec)
	if rec.Via != "beam" || rec.Deletes != 1 || rec.Username != "carol" || rec.ServerUUID != uuid {
		t.Fatalf("audit record = %+v", rec)
	}
}

// A session's own context can be cancelled by the client; a stale stamp read
// through it failed, and the failure counted as "Redis is down, allow".
func TestACancelledCallDoesNotSkipTheAccessCheck(t *testing.T) {
	prev := beamRecheckEvery
	beamRecheckEvery = 0
	t.Cleanup(func() { beamRecheckEvery = prev })
	const srv = "22222222-2222-2222-2222-222222222222"
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	bs := &beamServer{rdb: rdb}
	addr := "127.0.0.1:40400"
	iat := time.Now().Add(-time.Hour)
	bs.sessionByPeer.Store(addr, &beamSession{claims: &beamauth.BeamClaims{ServerUUID: srv, RegisteredClaims: jwt.RegisteredClaims{IssuedAt: jwt.NewNumericDate(iat)}}, opened: time.Now()})
	beamauth.BumpAccessEpoch(context.Background(), rdb, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if bs.sessionLive(ctx, addr) {
		t.Fatal("a cancelled call passed a stale session")
	}
}

// Sessions are capped so an access stamp can never age out from under one.
func TestABeamSessionEndsAfterItsMaximumAge(t *testing.T) {
	bs := &beamServer{}
	addr := "127.0.0.1:40500"
	bs.sessionByPeer.Store(addr, &beamSession{claims: &beamauth.BeamClaims{ServerUUID: "s"}, opened: time.Now().Add(-beamMaxSession - time.Minute)})
	if bs.sessionLive(context.Background(), addr) {
		t.Fatal("a session older than the maximum age was kept")
	}
}

// A negative size subtracted from what every other upload is counted against.
func TestABeamUploadCannotDeclareANegativeSize(t *testing.T) {
	bs, _, ctx := newTestBeamServer(t)
	stream := &fakeBeamUploadStream{ctx: ctx, msgs: []*pb.BeamUploadMsg{
		{Payload: &pb.BeamUploadMsg_Start{Start: &pb.BeamUploadStart{Path: "survival", Filename: "w.zip", TotalSize: -1 << 40}}},
	}}
	if err := bs.UploadFile(stream); err == nil {
		t.Fatal("a negative upload size was accepted")
	}
	if v, ok := bs.inflight.Load("s:11111111-1111-1111-1111-111111111111"); ok && v.(*atomic.Int64).Load() != 0 {
		t.Fatal("the negative size was counted")
	}
}
