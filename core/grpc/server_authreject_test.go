package nodegrpc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"testing"

	pb "dylaris-proto/node"

	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
)

// fakeNodeStream drives NodeConnect without a network. Embedding
// grpc.ServerStream satisfies the parts of the generated interface this path
// never touches; a call into any of them would nil-panic, which is the point.
type fakeNodeStream struct {
	grpc.ServerStream
	ctx  context.Context
	recv []*pb.NodeMessage
	next int
	sent []*pb.NodeMessage
}

func (f *fakeNodeStream) Context() context.Context { return f.ctx }

func (f *fakeNodeStream) Send(m *pb.NodeMessage) error {
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeNodeStream) Recv() (*pb.NodeMessage, error) {
	if f.next >= len(f.recv) {
		return nil, io.EOF
	}
	m := f.recv[f.next]
	f.next++
	return m, nil
}

// rejectingLookup makes every node unknown, which is what sends NodeConnect
// down the enrollment branch.
type rejectingLookup struct{}

func (rejectingLookup) GetNodeByToken(string) (*Node, error) {
	return nil, ErrNodeNotFound
}

// rejectingACL refuses the cluster proof and fails enrollment, so both doors in
// the unknown-node branch are shut.
type rejectingACL struct{}

func (rejectingACL) EnsureExisting(context.Context, int, string) (string, error) {
	return "", errors.New("not used")
}

func (rejectingACL) Enroll(context.Context, string, string, string) (string, int, string, error) {
	return "", 0, "", errors.New("enroll token not found")
}

func (rejectingACL) EnrollPlatform(context.Context, string, string) (string, int, string, error) {
	return "", 0, "", errors.New("not used")
}

func (rejectingACL) VerifyProof(context.Context, int, string, string) (bool, error) {
	return false, nil
}

func (rejectingACL) VerifyChallenge(context.Context, int, string, string) (bool, error) {
	return false, nil
}

func (rejectingACL) VerifyClusterProof(string, string) bool { return false }

func (rejectingACL) HasSecret(context.Context, int) (bool, error) { return false, nil }

func (rejectingACL) NodeKeys(context.Context, int) (ed25519.PublicKey, ed25519.PublicKey, error) {
	return nil, nil, nil
}

func (rejectingACL) SetNodeKey(context.Context, int, ed25519.PublicKey, ed25519.PublicKey, ed25519.PublicKey) (bool, error) {
	return true, nil
}

// connectWithAuth runs one NodeConnect against the fakes and returns whatever
// the standard logger emitted plus the messages the node received.
func connectWithAuth(t *testing.T, auth *pb.NodeAuth) (logged string, sent []*pb.NodeMessage) {
	t.Helper()

	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 51234},
	})
	stream := &fakeNodeStream{
		ctx:  ctx,
		recv: []*pb.NodeMessage{{Payload: &pb.NodeMessage_Auth{Auth: auth}}},
	}

	srv := NewServer(NewRegistry(), rejectingLookup{}, "core-test", rejectingACL{}, nil, nil)
	if err := srv.NodeConnect(stream); err == nil {
		t.Fatal("NodeConnect accepted a node it should have rejected")
	}
	return buf.String(), stream.sent
}

// TestNodeConnectLogsAuthRejections pins the reason this logging exists: gRPC
// has no interceptor here, so before this the authority side recorded nothing
// when it turned a node away. A node stuck out of the cluster was undebuggable
// from Core, and probing enroll tokens was invisible.
func TestNodeConnectLogsAuthRejections(t *testing.T) {
	// A 64-hex token, the shape a real node presents. The prefix must survive
	// into the log and the remainder must not.
	const token = "deadbeefcafef00d1122334455667788990011223344556677889900aabbccdd"

	tests := []struct {
		name       string
		auth       *pb.NodeAuth
		wantReason string
	}{
		{
			name:       "no enroll token and no cluster proof",
			auth:       &pb.NodeAuth{NodeToken: token},
			wantReason: "unknown node and no enroll token or cluster proof",
		},
		{
			name:       "enroll token present but invalid",
			auth:       &pb.NodeAuth{NodeToken: token, EnrollToken: "not-a-real-enroll-token"},
			wantReason: "enrollment failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logged, sent := connectWithAuth(t, tc.auth)

			if !strings.Contains(logged, tc.wantReason) {
				t.Errorf("Core log is missing the rejection reason %q\ngot: %q", tc.wantReason, logged)
			}
			if !strings.Contains(logged, "node auth rejected") {
				t.Errorf("Core log does not mark the line as an auth rejection\ngot: %q", logged)
			}
			// The peer IP is the point of the line for an operator: which box
			// is knocking. It is the TCP source, not the self-reported auth.Ips.
			if !strings.Contains(logged, "203.0.113.7") {
				t.Errorf("Core log is missing the peer IP\ngot: %q", logged)
			}
			if !strings.Contains(logged, token[:8]) {
				t.Errorf("Core log is missing the token prefix %q\ngot: %q", token[:8], logged)
			}
			// A node token is a credential. Logging it whole is the bug that
			// 6a7bd1b fixed elsewhere; this line must not reintroduce it.
			if strings.Contains(logged, token) {
				t.Error("Core log contains the WHOLE node token, want only the 8-char prefix")
			}

			// The node still has to be told why, unchanged by the logging.
			if len(sent) != 1 {
				t.Fatalf("sent %d messages to the node, want exactly 1 auth result", len(sent))
			}
			res := sent[0].GetAuthResult()
			if res == nil || res.Ok {
				t.Fatalf("node did not receive a failed AuthResult, got %+v", sent[0])
			}
			if res.Message != tc.wantReason {
				t.Errorf("node was told %q, want %q", res.Message, tc.wantReason)
			}
		})
	}
}

// acceptingClusterACL takes the cluster-proof door and records how many nodes
// it was asked to create.
type acceptingClusterACL struct {
	rejectingACL
	platformEnrolls int
}

func (a *acceptingClusterACL) VerifyClusterProof(string, string) bool { return true }

func (a *acceptingClusterACL) EnrollPlatform(context.Context, string, string) (string, int, string, error) {
	a.platformEnrolls++
	return "brand-new-uuid", 7, "aabb", nil
}

// A node that already HOLDS an identity must never be enrolled as a new one.
//
// It presents secret_proof, which only a node with a cached secret sends - so an
// unknown token plus a proof is a node whose row Core has LOST, not a new
// machine. Enrolling it there produced the worst possible outcome, measured on
// production 2026-08-31: Core minted a fresh identity, the node refused to adopt
// it (correctly - swapping identity at runtime orphans its servers and its Redis
// users), and thirty seconds later both did it again. 249 node rows, 119 of them
// in one hour, and the node never came back.
//
// The two sides now say the same thing: admit the node from Settings -> Nodes.
func TestAPairedNodeIsNeverReEnrolledUnderANewIdentity(t *testing.T) {
	acl := &acceptingClusterACL{}

	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 51234},
	})
	stream := &fakeNodeStream{ctx: ctx, recv: []*pb.NodeMessage{{Payload: &pb.NodeMessage_Auth{
		Auth: &pb.NodeAuth{
			NodeToken:    "d19b65e7-0bbf-43d1-9c62-374c3219733a",
			SecretProof:  "a-proof-of-the-cached-secret",
			ClusterProof: "a-valid-cluster-proof",
			AclSupported: true,
		},
	}}}}

	srv := NewServer(NewRegistry(), rejectingLookup{}, "core-test", acl, nil, nil)
	if err := srv.NodeConnect(stream); err == nil {
		t.Fatal("a node claiming an unknown identity was accepted")
	}
	if acl.platformEnrolls != 0 {
		t.Errorf("Core created %d new node(s) for a node that already had one; "+
			"that is the row-per-30-seconds leak", acl.platformEnrolls)
	}
	// The refusal has to name something the operator can actually DO. The first
	// version named "Reset pairing", which is a per-node action on a screen where
	// this node is not listed - a dead end reported on 2026-08-31 by the one
	// person following it. Clearing the cached identity is the way out of this
	// branch, so that is what it must say.
	msg := buf.String() + failMessages(stream.sent)
	if !strings.Contains(msg, ".node_secret") {
		t.Error("the refusal does not name the reachable action (clearing the cached identity)")
	}
}

// failMessages joins whatever reasons were sent back to the node.
func failMessages(sent []*pb.NodeMessage) string {
	var b strings.Builder
	for _, m := range sent {
		if ar := m.GetAuthResult(); ar != nil {
			b.WriteString(ar.GetMessage())
			b.WriteString(" ")
		}
	}
	return b.String()
}
