package main

import (
	"context"
	"net"
	"sync"
	"testing"

	beamauth "dylaris-pkg/beam/auth"
	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"google.golang.org/grpc/peer"
)

// A connection is bound to ONE ticket. The rights a file operation checks and
// the server it acts on are read in two steps, so a second Authenticate on the
// same connection let one operation take its rights from ticket A and its
// target from ticket B: a member with write on A and read-only on B wrote and
// deleted on B. The official client authenticates once per connection, so a
// second one is refused rather than allowed to replace the identity.
func TestABeamConnectionCannotReauthenticate(t *testing.T) {
	const secret = "test-beam-secret"
	const srvA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const srvB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	full := fileperms.Full()
	readOnly := fileperms.Perms{Read: true}
	sign := func(t *testing.T, srv string, p *fileperms.Perms) string {
		t.Helper()
		tok, err := beamauth.SignBeamTicket(secret, beamauth.BeamClaims{ServerUUID: srv, Username: "carol", Perms: p})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	cases := []struct {
		name   string
		second string
	}{
		{"a ticket for another server", sign(t, srvB, &readOnly)},
		{"a ticket for the same server", sign(t, srvA, &readOnly)},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bs := &beamServer{jwtSecret: secret}
			ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40600 + i}})
			if resp, err := bs.Authenticate(ctx, &pb.BeamAuthReq{Ticket: sign(t, srvA, &full)}); err != nil || !resp.Ok {
				t.Fatalf("first Authenticate: %v %v", resp, err)
			}
			resp, err := bs.Authenticate(ctx, &pb.BeamAuthReq{Ticket: c.second})
			if err != nil {
				t.Fatalf("second Authenticate: %v", err)
			}
			if resp.Ok {
				t.Fatal("a second Authenticate on the same connection was accepted")
			}
			if got := bs.extractServerUUID(ctx); got != srvA {
				t.Fatalf("server after a refused re-auth = %q, want %q", got, srvA)
			}
			if err := bs.requireFilePerm(ctx, canDelete, "delete"); err != nil {
				t.Fatalf("the first ticket's rights were lost: %v", err)
			}
		})
	}
}

// Two Authenticate calls racing on a fresh connection: exactly one wins, and
// the rights and the server the connection ends up with come from that one.
func TestConcurrentBeamAuthenticatesBindOneTicket(t *testing.T) {
	const secret = "test-beam-secret"
	const srvA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const srvB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	full := fileperms.Full()
	readOnly := fileperms.Perms{Read: true}
	tokA, _ := beamauth.SignBeamTicket(secret, beamauth.BeamClaims{ServerUUID: srvA, Perms: &full})
	tokB, _ := beamauth.SignBeamTicket(secret, beamauth.BeamClaims{ServerUUID: srvB, Perms: &readOnly})

	for i := 0; i < 200; i++ {
		bs := &beamServer{jwtSecret: secret}
		ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41000 + i}})
		var wg sync.WaitGroup
		oks := make([]bool, 2)
		for j, tok := range []string{tokA, tokB} {
			wg.Add(1)
			go func(j int, tok string) {
				defer wg.Done()
				resp, err := bs.Authenticate(ctx, &pb.BeamAuthReq{Ticket: tok})
				oks[j] = err == nil && resp.Ok
			}(j, tok)
		}
		wg.Wait()
		if oks[0] == oks[1] {
			t.Fatalf("round %d: accepted A=%v B=%v, want exactly one", i, oks[0], oks[1])
		}
		wantSrv, wantDelete := srvB, false
		if oks[0] {
			wantSrv, wantDelete = srvA, true
		}
		if got := bs.extractServerUUID(ctx); got != wantSrv {
			t.Fatalf("round %d: server %q, want %q", i, got, wantSrv)
		}
		if err := bs.requireFilePerm(ctx, canDelete, "delete"); (err == nil) != wantDelete {
			t.Fatalf("round %d: delete allowed=%v on %s, want %v", i, err == nil, wantSrv, wantDelete)
		}
	}
}
