package nodegrpc

import (
	"context"
	"errors"
	"net"
	"testing"

	"google.golang.org/grpc/peer"

	pb "dylaris-proto/node"
)

// knownNodeLookup is the other half of rejectingLookup: a node Core DOES have a
// row for, which is the only kind that can be admitted from the panel.
type knownNodeLookup struct {
	token string
	owned bool
}

func (k knownNodeLookup) GetNodeByToken(t string) (*Node, error) {
	if t != k.token {
		return nil, errors.New("no such node")
	}
	return &Node{ID: 42, Token: k.token, Owned: k.owned}, nil
}

// noSecretACL is a known node whose secret has been cleared - the state reset
// pairing and an approval both leave it in. VerifyClusterProof says no, so the
// only remaining door is the panel admission.
type noSecretACL struct{ rejectingACL }

func (noSecretACL) EnsureExisting(context.Context, int, string) (string, error) {
	return "aabbcc", nil
}

type recordingJoins struct {
	recorded      []JoinAttempt
	admitIP       string
	consumed      int
	forgot        []string
	authenticated []authRecord
}

type authRecord struct {
	nodeID int
	peerIP string
}

func (r *recordingJoins) RecordAuthenticated(nodeID int, peerIP string) error {
	r.authenticated = append(r.authenticated, authRecord{nodeID, peerIP})
	return nil
}

func (r *recordingJoins) RecordJoinAttempt(a JoinAttempt) error {
	r.recorded = append(r.recorded, a)
	return nil
}

func (r *recordingJoins) ConsumeJoinApproval(nodeToken, peerIP string) (bool, error) {
	// One-shot, and only from the address the approval was granted for - the
	// same two properties the real store enforces in SQL.
	if r.admitIP == "" || peerIP != r.admitIP {
		return false, nil
	}
	r.admitIP = ""
	r.consumed++
	return true, nil
}

func (r *recordingJoins) ForgetJoinAttempts(t string) error {
	r.forgot = append(r.forgot, t)
	return nil
}

func connectKnownNode(t *testing.T, joins JoinAttemptRecorder, fromIP string, auth *pb.NodeAuth) error {
	t.Helper()
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP(fromIP), Port: 51234},
	})
	stream := &fakeNodeStream{
		ctx:  ctx,
		recv: []*pb.NodeMessage{{Payload: &pb.NodeMessage_Auth{Auth: auth}}},
	}
	srv := NewServer(NewRegistry(), knownNodeLookup{token: auth.NodeToken}, "core-test", noSecretACL{}, nil, nil, joins)
	return srv.NodeConnect(stream)
}

func authFor(token string) *pb.NodeAuth {
	return &pb.NodeAuth{
		NodeToken: token,
		Ips:       &pb.NodeIPs{Public: "198.51.100.9", Private: []string{"10.0.0.4"}},
		Identity: &pb.NodeIdentity{
			Hostname: "eu-node-00", CpuCores: 4, CpuModel: "Test CPU", MemoryBytes: 8 << 30,
		},
		ReleaseVersion: "2026.09.09",
	}
}

// The refusal has to leave a trace an operator can find. Before this it was one
// line on Core's stdout, which dies with the container - so a node retrying
// every thirty seconds for six hours was invisible in the panel and looked
// exactly like a machine that had been switched off.
func TestARefusedKnownNodeIsRecordedWithWhatItClaimsToBe(t *testing.T) {
	joins := &recordingJoins{}
	if err := connectKnownNode(t, joins, "203.0.113.7", authFor("node-abc")); err == nil {
		t.Fatal("a node with no secret, no cluster proof and no approval was admitted")
	}
	if len(joins.recorded) != 1 {
		t.Fatalf("recorded %d attempts, want 1", len(joins.recorded))
	}
	got := joins.recorded[0]
	// The observed address is the one field the caller cannot choose, so it is
	// the one the admission is later bound to.
	if got.PeerIP != "203.0.113.7" {
		t.Errorf("PeerIP = %q, want the observed source address", got.PeerIP)
	}
	if got.Hostname != "eu-node-00" || got.CPUCores != 4 || got.MemoryBytes != 8<<30 {
		t.Errorf("the self-reported identity did not reach the record: %+v", got)
	}
	if got.PublicIP != "198.51.100.9" || got.PrivateIPs != "10.0.0.4" {
		t.Errorf("the reported addresses did not reach the record: %+v", got)
	}
	if got.Reason == "" {
		t.Error("no reason recorded; the panel would show a refusal with no cause")
	}
}

// The point of the whole change: a machine gets back in without anyone touching
// it. Approving arms Core; the node's next retry is admitted.
func TestAnApprovedNodeIsAdmittedOnItsNextAttempt(t *testing.T) {
	joins := &recordingJoins{admitIP: "203.0.113.7"}
	if err := connectKnownNode(t, joins, "203.0.113.7", authFor("node-abc")); err != nil {
		t.Fatalf("an approved node was still refused: %v", err)
	}
	if joins.consumed != 1 {
		t.Errorf("the approval was consumed %d times, want exactly 1", joins.consumed)
	}
	// Back in, so it is no longer something to act on.
	if len(joins.forgot) != 1 || joins.forgot[0] != "node-abc" {
		t.Errorf("the refusal record was not cleared after a successful connect: %v", joins.forgot)
	}
}

// An approval is for the machine the operator was LOOKING at. The identity on a
// refused attempt is self-claimed, so anyone who learns a node's id can knock -
// binding the approval to the observed address is what stops the next knock
// walking through a door opened for somebody else.
func TestAnApprovalDoesNotAdmitTheSameIdentityFromAnotherAddress(t *testing.T) {
	joins := &recordingJoins{admitIP: "203.0.113.7"}
	if err := connectKnownNode(t, joins, "198.51.100.200", authFor("node-abc")); err == nil {
		t.Fatal("an approval granted for one address admitted a connection from another")
	}
	if joins.consumed != 0 {
		t.Error("the approval was consumed by a connection it does not cover")
	}
	// And it is still armed for the address it was granted for.
	if joins.admitIP != "203.0.113.7" {
		t.Error("a refused attempt from elsewhere disarmed the operator's approval")
	}
}

// provisionedACL is a known node whose secret Core holds, answering the
// challenge the way verdict says.
type provisionedACL struct {
	noSecretACL
	verdict bool
}

func (provisionedACL) HasSecret(context.Context, int) (bool, error) { return true, nil }
func (a provisionedACL) VerifyChallenge(context.Context, int, string, string) (bool, error) {
	return a.verdict, nil
}

// runConnect drives one NodeConnect from fromIP with the given messages.
func runConnect(t *testing.T, lookup NodeLookup, acl ACLHandshake, joins JoinAttemptRecorder, fromIP string, msgs ...*pb.NodeMessage) error {
	t.Helper()
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP(fromIP), Port: 51234},
	})
	srv := NewServer(NewRegistry(), lookup, "core-test", acl, nil, nil, joins)
	return srv.NodeConnect(&fakeNodeStream{ctx: ctx, recv: msgs})
}

func authMsg(a *pb.NodeAuth) *pb.NodeMessage {
	return &pb.NodeMessage{Payload: &pb.NodeMessage_Auth{Auth: a}}
}

// The roll-key action binds the admission it arms to the address of the node's
// last SUCCESSFUL authentication. Every way in has to leave that address, and it
// has to be the socket's: authFor reports 198.51.100.9 as the node's public IP,
// so a recorder fed the self-reported value would fail here.
func TestEverySuccessfulAuthenticationRecordsTheObservedAddress(t *testing.T) {
	cases := []struct {
		name   string
		lookup NodeLookup
		acl    ACLHandshake
		joins  *recordingJoins
		msgs   []*pb.NodeMessage
		wantID int
	}{
		{
			name:   "challenge verified",
			lookup: knownNodeLookup{token: "node-abc"},
			acl:    provisionedACL{verdict: true},
			joins:  &recordingJoins{},
			msgs: []*pb.NodeMessage{
				authMsg(authFor("node-abc")),
				{Payload: &pb.NodeMessage_ChallengeResponse{ChallengeResponse: &pb.NodeChallengeResponse{Response: "ok"}}},
			},
			wantID: 42,
		},
		{
			name:   "first issuance through a panel admission",
			lookup: knownNodeLookup{token: "node-abc"},
			acl:    noSecretACL{},
			joins:  &recordingJoins{admitIP: "203.0.113.7"},
			msgs:   []*pb.NodeMessage{authMsg(authFor("node-abc"))},
			wantID: 42,
		},
		{
			name:   "new node through the cluster proof",
			lookup: rejectingLookup{},
			acl:    &acceptingClusterACL{},
			joins:  &recordingJoins{},
			msgs:   []*pb.NodeMessage{authMsg(authFor("eu-node-00"))},
			wantID: 7,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := runConnect(t, tc.lookup, tc.acl, tc.joins, "203.0.113.7", tc.msgs...); err != nil {
				t.Fatalf("the node was refused: %v", err)
			}
			want := []authRecord{{tc.wantID, "203.0.113.7"}}
			if len(tc.joins.authenticated) != 1 || tc.joins.authenticated[0] != want[0] {
				t.Errorf("recorded %v, want %v", tc.joins.authenticated, want)
			}
		})
	}
}

// A refused node has proved nothing, so its address must not become the one a
// later roll-key admission is bound to.
func TestARefusedNodeRecordsNoAuthenticatedAddress(t *testing.T) {
	t.Run("wrong challenge response", func(t *testing.T) {
		joins := &recordingJoins{}
		err := runConnect(t, knownNodeLookup{token: "node-abc"}, provisionedACL{verdict: false}, joins, "203.0.113.7",
			authMsg(authFor("node-abc")),
			&pb.NodeMessage{Payload: &pb.NodeMessage_ChallengeResponse{ChallengeResponse: &pb.NodeChallengeResponse{Response: "bad"}}})
		if err == nil {
			t.Fatal("a wrong challenge response was accepted")
		}
		if len(joins.authenticated) != 0 {
			t.Errorf("recorded %v for a node that failed its challenge", joins.authenticated)
		}
	})
	t.Run("no secret and no admission", func(t *testing.T) {
		joins := &recordingJoins{}
		if err := connectKnownNode(t, joins, "203.0.113.7", authFor("node-abc")); err == nil {
			t.Fatal("the node was admitted without any credential")
		}
		if len(joins.authenticated) != 0 {
			t.Errorf("recorded %v for a refused node", joins.authenticated)
		}
	})
}

// A node with no identity block - an older image - must still be recorded, or
// upgrading Core first would hide exactly the nodes that need attention.
func TestAnOlderNodeWithNoIdentityIsStillRecorded(t *testing.T) {
	joins := &recordingJoins{}
	auth := &pb.NodeAuth{NodeToken: "node-abc"}
	if err := connectKnownNode(t, joins, "203.0.113.7", auth); err == nil {
		t.Fatal("the node was admitted without any credential")
	}
	if len(joins.recorded) != 1 {
		t.Fatalf("recorded %d attempts, want 1", len(joins.recorded))
	}
	if joins.recorded[0].Hostname != "" || joins.recorded[0].CPUCores != 0 {
		t.Error("fields were invented for a node that reported none")
	}
}
