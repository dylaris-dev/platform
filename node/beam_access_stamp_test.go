package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/peer"

	beamauth "dylaris-pkg/beam/auth"
	pb "dylaris-proto/beam"
)

// A beam ticket is a bearer token with a 30 minute life and nothing re-read it,
// so taking someone's access away stopped NEW tickets and left an outstanding
// one working to its expiry - the same for a suspended account, whose minting
// is refused while a ticket it already holds is not.
//
// Core stamps the server when that changes. This is the node half: a ticket
// minted before the stamp does not open a session.
func TestAuthenticateRefusesATicketOlderThanTheAccessStamp(t *testing.T) {
	const secret = "test-beam-secret"
	const srv = "22222222-2222-2222-2222-222222222222"

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40200}})

	sign := func(t *testing.T) string {
		t.Helper()
		tok, err := beamauth.SignBeamTicket(secret, beamauth.BeamClaims{ServerUUID: srv, Username: "carol"})
		if err != nil {
			t.Fatalf("SignBeamTicket: %v", err)
		}
		return tok
	}

	bs := &beamServer{jwtSecret: secret, rdb: rdb}

	t.Run("no stamp, the ticket works", func(t *testing.T) {
		resp, err := bs.Authenticate(ctx, &pb.BeamAuthReq{Ticket: sign(t)})
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if !resp.Ok {
			t.Fatalf("refused without a stamp: %s", resp.Message)
		}
	})

	stale := sign(t)
	// The stamp lands after the ticket was minted. A second of daylight, because
	// the claim carries whole seconds.
	time.Sleep(1100 * time.Millisecond)
	if err := beamauth.BumpAccessEpoch(context.Background(), rdb, srv); err != nil {
		t.Fatalf("bump: %v", err)
	}

	t.Run("the older ticket is refused", func(t *testing.T) {
		resp, err := bs.Authenticate(ctx, &pb.BeamAuthReq{Ticket: stale})
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if resp.Ok {
			t.Fatal("a ticket minted before the access change opened a session")
		}
		// The holder has to be told what to DO, or they retry the same ticket.
		if resp.Message == "" {
			t.Error("refused with no message")
		}
	})

	t.Run("a ticket minted after it works again", func(t *testing.T) {
		time.Sleep(1100 * time.Millisecond)
		resp, err := bs.Authenticate(ctx, &pb.BeamAuthReq{Ticket: sign(t)})
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if !resp.Ok {
			t.Fatalf("reconnecting with a fresh ticket was refused: %s", resp.Message)
		}
	})

	// The node must not lock a customer out of their own files because Redis
	// blinked. It reopens the window that existed before, which is the lesser
	// of the two failures.
	t.Run("an unreachable Redis lets the ticket through", func(t *testing.T) {
		mr.Close()
		resp, err := bs.Authenticate(ctx, &pb.BeamAuthReq{Ticket: sign(t)})
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if !resp.Ok {
			t.Fatalf("a Redis outage refused a valid ticket: %s", resp.Message)
		}
	})
}
