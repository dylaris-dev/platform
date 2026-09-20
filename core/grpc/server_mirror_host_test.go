package nodegrpc

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc/peer"

	pb "dylaris-proto/node"
)

func TestMirrorHostFromURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https origin", "https://panel.example.com", "panel.example.com"},
		{"trailing path is dropped", "https://panel.example.com/base/", "panel.example.com"},
		{"port is part of the host", "https://panel.example.com:8443", "panel.example.com:8443"},
		{"http is a host too", "http://core.internal:8080", "core.internal:8080"},
		{"case folded", "https://Panel.Example.COM", "panel.example.com"},
		{"padded", "  https://panel.example.com  ", "panel.example.com"},
		{"unset", "", ""},
		{"no scheme is not a URL we can trust", "panel.example.com", ""},
		{"a scheme we never download over", "ftp://panel.example.com", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MirrorHostFromURL(c.in); got != c.want {
				t.Errorf("MirrorHostFromURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A node learns the host its pack downloads come from HERE and nowhere else, so
// every door into Core has to say it - including a BYON node's, which is the
// one this matters most for: nothing on a customer's machine can be configured
// with it.
func TestEverySuccessfulAuthResultCarriesCoresMirrorHost(t *testing.T) {
	enroll := authFor("byon-box")
	enroll.EnrollToken = "a-valid-enroll-token"

	cases := []struct {
		name      string
		lookup    NodeLookup
		acl       ACLHandshake
		joins     *recordingJoins
		msgs      []*pb.NodeMessage
		publicURL func() string
		want      string
	}{
		{
			name:   "reconnect after the challenge",
			lookup: knownNodeLookup{token: "node-abc"},
			acl:    provisionedACL{verdict: true},
			msgs: []*pb.NodeMessage{
				authMsg(authFor("node-abc")),
				{Payload: &pb.NodeMessage_ChallengeResponse{ChallengeResponse: &pb.NodeChallengeResponse{Response: "ok"}}},
			},
			publicURL: func() string { return "https://panel.example.com/" },
			want:      "panel.example.com",
		},
		{
			name:      "new enrolment through a tenant's enroll token",
			lookup:    rejectingLookup{},
			acl:       acceptingEnrollACL{},
			msgs:      []*pb.NodeMessage{authMsg(enroll)},
			publicURL: func() string { return "https://panel.example.com" },
			want:      "panel.example.com",
		},
		{
			name:      "new enrolment through the cluster proof",
			lookup:    rejectingLookup{},
			acl:       &acceptingClusterACL{},
			msgs:      []*pb.NodeMessage{authMsg(authFor("eu-node-00"))},
			publicURL: func() string { return "https://panel.example.com" },
			want:      "panel.example.com",
		},
		{
			// A Core with no public URL configured names no host. The node reads
			// that as "keep what you have", so it must be the empty string and
			// not a half-built one.
			name:      "core_public_url is not set",
			lookup:    knownNodeLookup{token: "node-abc"},
			acl:       provisionedACL{verdict: true},
			msgs:      []*pb.NodeMessage{authMsg(authFor("node-abc")), {Payload: &pb.NodeMessage_ChallengeResponse{ChallengeResponse: &pb.NodeChallengeResponse{Response: "ok"}}}},
			publicURL: func() string { return "" },
			want:      "",
		},
		{
			name:   "no source wired at all",
			lookup: knownNodeLookup{token: "node-abc"},
			acl:    provisionedACL{verdict: true},
			msgs:   []*pb.NodeMessage{authMsg(authFor("node-abc")), {Payload: &pb.NodeMessage_ChallengeResponse{ChallengeResponse: &pb.NodeChallengeResponse{Response: "ok"}}}},
			want:   "",
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
			srv.SetPublicURLFunc(tc.publicURL)
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
			if ok.ModpackMirrorHost != tc.want {
				t.Errorf("ModpackMirrorHost = %q, want %q", ok.ModpackMirrorHost, tc.want)
			}
		})
	}
}
