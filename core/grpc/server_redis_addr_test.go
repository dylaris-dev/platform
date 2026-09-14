package nodegrpc

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc/peer"

	pb "dylaris-proto/node"
)

// acceptingEnrollACL takes the enroll-token door. A tenant's token produces an
// owned (BYON) node; platform makes it an admin's External node token, whose
// row is born unowned.
type acceptingEnrollACL struct {
	rejectingACL
	platform bool
}

func (a acceptingEnrollACL) Enroll(context.Context, string, string, string) (string, int, string, bool, error) {
	return "enrolled-uuid", 9, "aabb", !a.platform, nil
}

// The node no longer carries a Redis address of its own; it is told one here.
// So every way in has to say it, or a node that took that door boots with no
// address - and an owned node must NOT be told it: it reaches Redis through its
// warp proxy, and Core's internal service name means nothing on that machine.
func TestEverySuccessfulAuthResultCarriesCoresRedisAddr(t *testing.T) {
	const coreRedis = "redis:6379"
	enroll := authFor("byon-box")
	enroll.EnrollToken = "a-valid-enroll-token"

	cases := []struct {
		name   string
		lookup NodeLookup
		acl    ACLHandshake
		joins  *recordingJoins
		msgs   []*pb.NodeMessage
		want   string
	}{
		{
			name:   "reconnect after the challenge",
			lookup: knownNodeLookup{token: "node-abc"},
			acl:    provisionedACL{verdict: true},
			msgs: []*pb.NodeMessage{
				authMsg(authFor("node-abc")),
				{Payload: &pb.NodeMessage_ChallengeResponse{ChallengeResponse: &pb.NodeChallengeResponse{Response: "ok"}}},
			},
			want: coreRedis,
		},
		{
			name:   "first issuance for a known node",
			lookup: knownNodeLookup{token: "node-abc"},
			acl:    noSecretACL{},
			joins:  &recordingJoins{admitIP: "203.0.113.7"},
			msgs:   []*pb.NodeMessage{authMsg(authFor("node-abc"))},
			want:   coreRedis,
		},
		{
			name:   "new enrolment through the cluster proof",
			lookup: rejectingLookup{},
			acl:    &acceptingClusterACL{},
			msgs:   []*pb.NodeMessage{authMsg(authFor("eu-node-00"))},
			want:   coreRedis,
		},
		{
			name:   "new enrolment through a tenant's enroll token is an owned node",
			lookup: rejectingLookup{},
			acl:    acceptingEnrollACL{},
			msgs:   []*pb.NodeMessage{authMsg(enroll)},
			want:   "",
		},
		{
			// Told what its reconnects will be told, since the row it gets is
			// unowned. Harmless on that machine: a node follows Core's address
			// only when it holds CLUSTER_SECRET (node/redis_addr.go).
			name:   "new enrolment through a platform token is an unowned node",
			lookup: rejectingLookup{},
			acl:    acceptingEnrollACL{platform: true},
			msgs:   []*pb.NodeMessage{authMsg(enroll)},
			want:   coreRedis,
		},
		{
			name:   "reconnect of an owned node",
			lookup: knownNodeLookup{token: "node-abc", owned: true},
			acl:    provisionedACL{verdict: true},
			msgs: []*pb.NodeMessage{
				authMsg(authFor("node-abc")),
				{Payload: &pb.NodeMessage_ChallengeResponse{ChallengeResponse: &pb.NodeChallengeResponse{Response: "ok"}}},
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := peer.NewContext(context.Background(), &peer.Peer{
				Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 51234},
			})
			var joins JoinAttemptRecorder
			if tc.joins != nil {
				joins = tc.joins
			}
			srv := NewServer(NewRegistry(), tc.lookup, "core-test", tc.acl, nil, joins)
			srv.SetRedisAddr(coreRedis)
			stream := &fakeNodeStream{ctx: ctx, recv: tc.msgs}
			if err := srv.NodeConnect(stream); err != nil {
				t.Fatalf("the node was refused: %v", err)
			}
			var ok *pb.AuthResult
			for _, m := range stream.sent {
				if ar := m.GetAuthResult(); ar != nil && ar.Ok {
					ok = ar
				}
			}
			if ok == nil {
				t.Fatal("no successful AuthResult was sent")
			}
			if ok.RedisAddr != tc.want {
				t.Errorf("RedisAddr = %q, want %q", ok.RedisAddr, tc.want)
			}
		})
	}
}
