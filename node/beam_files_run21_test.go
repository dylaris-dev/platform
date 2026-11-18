package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	beamauth "dylaris-pkg/beam/auth"
	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// A beam upload over an existing file only grows the server by the difference,
// as the panel upload, SFTP and SaveFileContent already count it. Start used to
// charge the whole new size, so a near-full server could not replace a file.
func TestABeamUploadOverAFileIsChargedOnlyTheGrowth(t *testing.T) {
	const size = 100
	cases := []struct {
		name     string
		existing string // file already at survival/<name>, "" for none
		target   string
		wantCode codes.Code
	}{
		{"replacing a file of the same size fits", "w.zip", "w.zip", codes.OK},
		{"a new file of that size does not", "w.zip", "other.zip", codes.ResourceExhausted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bs, uuid, ctx := newTestBeamServer(t)
			mr := miniredis.RunT(t)
			bs.rdb = redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { bs.rdb.Close() })
			// 120 used of 150, 100 of which is the file being replaced.
			bs.rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":120,"limit":150}`, 10*time.Minute)
			dir := filepath.Join(bs.storageMgr.GetServerDir(uuid), "survival")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, tc.existing), bytes.Repeat([]byte("o"), size), 0o644); err != nil {
				t.Fatal(err)
			}
			stream := &fakeBeamUploadStream{ctx: ctx, msgs: []*pb.BeamUploadMsg{
				{Payload: &pb.BeamUploadMsg_Start{Start: &pb.BeamUploadStart{Path: "survival", Filename: tc.target, TotalSize: size}}},
				{Payload: &pb.BeamUploadMsg_Chunk{Chunk: &pb.BeamUploadChunkData{Data: bytes.Repeat([]byte("n"), size)}}},
			}}
			if err := bs.UploadFile(stream); status.Code(err) != tc.wantCode {
				t.Fatalf("err = %v, want code %v", err, tc.wantCode)
			}
		})
	}
}

// An Authenticate still running when its connection ended stored its binding
// after the ConnEnd cleaner had already run, so the binding outlived the
// connection and belonged to the next one that reused the peer address.
func TestAnAuthenticateThatOutlivesItsConnectionLeavesNoBinding(t *testing.T) {
	const secret = "test-beam-secret"
	full := fileperms.Full()
	tok, err := beamauth.SignBeamTicket(secret, beamauth.BeamClaims{ServerUUID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Username: "carol", Perms: &full})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		ended  bool
		wantOk bool
	}{
		{"a live connection is bound", false, true},
		{"an ended connection is not", true, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bs := &beamServer{jwtSecret: secret}
			addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40700 + i}
			ctx, cancel := context.WithCancel(peer.NewContext(context.Background(), &peer.Peer{Addr: addr}))
			defer cancel()
			if c.ended {
				cancel()
			}
			resp, err := bs.Authenticate(ctx, &pb.BeamAuthReq{Ticket: tok})
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if resp.Ok != c.wantOk {
				t.Fatalf("Ok = %v, want %v", resp.Ok, c.wantOk)
			}
			for name, m := range map[string]*sync.Map{"session": &bs.sessionByPeer, "server": &bs.serverUUIDByPeer, "user": &bs.usernameByPeer, "perms": &bs.permsByPeer} {
				if _, bound := m.Load(addr.String()); bound != c.wantOk {
					t.Errorf("%s binding present = %v, want %v", name, bound, c.wantOk)
				}
			}
		})
	}
}
