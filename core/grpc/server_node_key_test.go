package nodegrpc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"dylaris-core/services/redisacl"
	"dylaris-pkg/nodeauth"
	pb "dylaris-proto/node"

	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
)

// keyedACL is one known node's credentials as Core holds them - a service
// secret, a login key and a rejected key - checked with the real derivations, so
// the node side of these tests signs and HMACs exactly as a node does.
type keyedACL struct {
	rejectingACL
	secret        []byte // nil: the row holds no secret
	key, rejected ed25519.PublicKey
	clusterOK     bool
	// raceTo, when set, is a key another Core replica stores for this row just
	// before this one writes, as happens when a node dials every replica at once.
	raceTo ed25519.PublicKey
	// afterStore, when set, runs right after a key write lands: an operator
	// acting on the row in the moment after it.
	afterStore func(a *keyedACL)
	onVerify   func()

	stored          []ed25519.PublicKey // every SetNodeKey that landed, in order
	storedFor       []int
	platformEnrolls int
	enrolls         int
}

func (a *keyedACL) HasSecret(context.Context, int) (bool, error) { return a.secret != nil, nil }

// VerifyChallenge reads the secret as it is at the call, as the store does.
// onVerify runs after each check: an operator acting right after it.
func (a *keyedACL) VerifyChallenge(_ context.Context, _ int, nonce, resp string) (bool, error) {
	ok := a.secret != nil && redisacl.VerifyChallenge(a.secret, nonce, resp)
	if a.onVerify != nil {
		a.onVerify()
	}
	return ok, nil
}

func (a *keyedACL) NodeKeys(context.Context, int) (ed25519.PublicKey, ed25519.PublicKey, error) {
	return a.key, a.rejected, nil
}

// SetNodeKey is the compare-and-set the store does in SQL.
func (a *keyedACL) SetNodeKey(_ context.Context, id int, prevKey, prevRejected, k ed25519.PublicKey) (bool, error) {
	if a.raceTo != nil {
		a.key, a.raceTo = a.raceTo, nil
	}
	if !bytes.Equal(a.key, prevKey) || !bytes.Equal(a.rejected, prevRejected) {
		return false, nil
	}
	a.stored = append(a.stored, k)
	a.storedFor = append(a.storedFor, id)
	a.key = k
	if a.afterStore != nil {
		a.afterStore(a)
	}
	return true, nil
}

// resetPairing and rollKey are what the two admin actions do to the row:
// both move the key aside, and only Reset clears the secret.
func resetPairing(a *keyedACL) {
	if a.key != nil {
		a.rejected = a.key
	}
	a.key, a.secret = nil, nil
}

func rollKey(a *keyedACL) {
	if a.key != nil {
		a.rejected = a.key
	}
	a.key = nil
}

func (a *keyedACL) VerifyClusterProof(string, string) bool { return a.clusterOK }

// EnsureExisting mints only when the row holds no secret, as
// LoadOrCreateNodeSecret does, and otherwise returns the stored one.
func (a *keyedACL) EnsureExisting(context.Context, int, string) (string, error) {
	if a.secret == nil {
		a.secret = bytes.Repeat([]byte{0x42}, 32)
	}
	return hex.EncodeToString(a.secret), nil
}

func (a *keyedACL) EnrollPlatform(context.Context, string, string) (string, int, string, error) {
	a.platformEnrolls++
	return "brand-new-uuid", 7, "aabb", nil
}

func (a *keyedACL) Enroll(context.Context, string, string, string) (string, int, string, error) {
	a.enrolls++
	return "owned-uuid", 9, "aabb", nil
}

// fakeNode plays a node over the stream: it sends its auth first and answers a
// challenge with every proof it holds, the way the node's recvAuthResult does.
type fakeNode struct {
	grpc.ServerStream
	ctx        context.Context
	auth       *pb.NodeAuth
	secret     []byte             // nil: answers no HMAC
	signWith   ed25519.PrivateKey // nil: answers no signature
	signAs     string             // the identity the signature binds
	pending    []*pb.NodeMessage
	sent       []*pb.NodeMessage
	challenges int
	// onChallenge runs while Core's challenge is out and the node has not yet
	// answered: the window a caller can hold open for as long as it likes.
	onChallenge func()
	// reg is the registry Core registers into; registered says whether the node
	// was in it when Core started reading its stream, which only a
	// registered node reaches.
	reg        *Registry
	registered bool
}

func (n *fakeNode) Context() context.Context { return n.ctx }

func (n *fakeNode) Send(m *pb.NodeMessage) error {
	n.sent = append(n.sent, m)
	if ch := m.GetChallenge(); ch != nil {
		n.challenges++
		if n.onChallenge != nil {
			n.onChallenge()
		}
		cr := &pb.NodeChallengeResponse{}
		if n.secret != nil {
			cr.Response = redisacl.ChallengeResponse(n.secret, ch.Nonce)
		}
		if n.signWith != nil {
			cr.Signature = nodeauth.SignChallenge(n.signWith, n.signAs, ch.Nonce)
		}
		n.pending = append(n.pending, &pb.NodeMessage{Payload: &pb.NodeMessage_ChallengeResponse{ChallengeResponse: cr}})
	}
	return nil
}

func (n *fakeNode) Recv() (*pb.NodeMessage, error) {
	if n.auth != nil {
		a := n.auth
		n.auth = nil
		return authMsg(a), nil
	}
	if len(n.pending) == 0 {
		// Core reads again after the handshake only from its read loop, which
		// runs after Register. 42 is the known row; 7 and 9 are what the
		// enrolment fakes create.
		if n.reg != nil {
			n.registered = n.registered || n.reg.IsConnected(42) || n.reg.IsConnected(7) || n.reg.IsConnected(9)
		}
		return nil, io.EOF
	}
	m := n.pending[0]
	n.pending = n.pending[1:]
	return m, nil
}

// result is the AuthResult Core sent, nil when it sent none.
func (n *fakeNode) result() *pb.AuthResult {
	for _, m := range n.sent {
		if ar := m.GetAuthResult(); ar != nil {
			return ar
		}
	}
	return nil
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return k
}

func pubOf(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

// asNode is a node sending a, answering with secret and signWith, and signing
// for the identity a presents, as a real node does.
func asNode(a *pb.NodeAuth, secret []byte, signWith ed25519.PrivateKey) *fakeNode {
	return &fakeNode{auth: a, secret: secret, signWith: signWith, signAs: a.NodeToken}
}

// nodeWith is node-abc presenting present (nil: no key), holding secret (nil:
// none, so it sends no secret proof, as a node without one never does) and
// signing with signWith.
func nodeWith(present ed25519.PublicKey, secret []byte, signWith ed25519.PrivateKey) *fakeNode {
	a := authFor("node-abc")
	a.NodePublicKey = present
	if secret != nil {
		a.SecretProof = "a-proof-of-the-cached-secret"
	}
	return asNode(a, secret, signWith)
}

// dial runs one NodeConnect of n from 203.0.113.7.
func dial(t *testing.T, lookup NodeLookup, acl ACLHandshake, joins JoinAttemptRecorder, n *fakeNode) error {
	t.Helper()
	n.ctx = peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 51234},
	})
	n.reg = NewRegistry()
	return NewServer(n.reg, lookup, "core-test", acl, nil, joins).NodeConnect(n)
}

var (
	known     = knownNodeLookup{token: "node-abc"}              // node 42, a platform node
	customer  = knownNodeLookup{token: "node-abc", owned: true} // node 42, a BYON node
	rowSecret = bytes.Repeat([]byte{0x11}, 32)
)

// A row that holds a key logs its node in by a signature over Core's nonce.
func TestAKeyRowLogsInWithItsKey(t *testing.T) {
	k := newKey(t)
	acl := &keyedACL{secret: rowSecret, key: pubOf(k)}
	joins := &recordingJoins{}
	n := nodeWith(pubOf(k), rowSecret, k)

	if err := dial(t, known, acl, joins, n); err != nil {
		t.Fatalf("a node signing with its registered key was refused: %v", err)
	}
	res := n.result()
	if res == nil || !res.Ok {
		t.Fatalf("auth result = %+v, want ok", res)
	}
	if res.NodeSecret != "" {
		t.Error("a node that proved it holds its secret was handed it again")
	}
	if len(acl.stored) != 0 || len(joins.recorded) != 0 {
		t.Errorf("stored %d key(s), recorded %d refusal(s); want neither", len(acl.stored), len(joins.recorded))
	}
	// The positive control for every "not registered" below.
	if !n.registered {
		t.Error("a node that logged in was not found in the registry")
	}
}

// An operator action that lands while a login waits on the node's answer wins.
// The premise is the one Reset exists for - someone else holds the node's key -
// and the caller holds its answer back until the operator has acted. Answering
// afterwards must not bring the replaced key back in, hand out the secret (a new
// one after a Reset, the stored one after a Roll) or take the registry slot.
func TestAnOperatorActionDuringTheLoginWins(t *testing.T) {
	for _, act := range []struct {
		name string
		do   func(*keyedACL)
	}{{"reset pairing", resetPairing}, {"roll key", rollKey}} {
		for _, row := range []struct {
			name   string
			lookup knownNodeLookup
		}{{"platform node", known}, {"customer node", customer}} {
			for _, proof := range []bool{false, true} {
				name := act.name + ", " + row.name
				if proof {
					name += ", with a secret proof"
				}
				t.Run(name, func(t *testing.T) {
					k := newKey(t)
					acl := &keyedACL{secret: rowSecret, key: pubOf(k), clusterOK: true}
					joins := &recordingJoins{}
					var secret []byte
					if proof {
						secret = rowSecret
					}
					n := nodeWith(pubOf(k), secret, k)
					n.onChallenge = func() { act.do(acl) }

					err := dial(t, row.lookup, acl, joins, n)
					res := n.result()
					t.Logf("err=%v ok=%v handed=%q rowKeyNow=%x registered=%v",
						err, res.GetOk(), res.GetNodeSecret(), acl.key, n.registered)
					if err == nil || res == nil || res.Ok {
						t.Fatal("the login survived the operator's action")
					}
					if res.NodeSecret != "" {
						t.Errorf("handed out %q", res.NodeSecret)
					}
					if n.registered || len(joins.authenticated) != 0 {
						t.Error("the caller was registered")
					}
					if len(joins.recorded) != 1 {
						t.Errorf("recorded %d refusals, want 1", len(joins.recorded))
					}
					if acl.key != nil || len(acl.stored) != 0 {
						t.Errorf("the row's key is %x after the operator moved it aside", acl.key)
					}
				})
			}
		}
	}

	// A row that never had a key logs in by its secret. A Reset that empties
	// the row after the answer was checked makes EnsureExisting mint a new
	// secret; the answer no longer verifies against it.
	t.Run("reset pairing of a secret-only row after the answer was checked", func(t *testing.T) {
		acl := &keyedACL{secret: rowSecret}
		joins := &recordingJoins{}
		n := nodeWith(nil, rowSecret, nil)
		verified := false
		acl.onVerify = func() {
			if !verified {
				verified = true
				resetPairing(acl)
			}
		}
		if err := dial(t, known, acl, joins, n); err == nil {
			t.Fatal("the secret login survived the Reset")
		}
		if res := n.result(); res == nil || res.Ok || res.NodeSecret != "" || n.registered {
			t.Errorf("result %+v, registered %v; want a refusal that hands and registers nothing", res, n.registered)
		}
	})

	// The same last check covers a key the re-pair branch has just stored. The
	// row starts with its key moved aside: a live key is never re-paired by a
	// cluster proof, so this is the only state that reaches the store.
	t.Run("roll key right after a re-pair stored its key", func(t *testing.T) {
		old, fresh := newKey(t), newKey(t)
		acl := &keyedACL{secret: rowSecret, rejected: pubOf(old), clusterOK: true, afterStore: rollKey}
		n := nodeWith(pubOf(fresh), nil, fresh)
		if err := dial(t, known, acl, &recordingJoins{}, n); err == nil {
			t.Fatal("the re-pair survived the Roll")
		}
		if res := n.result(); res == nil || res.Ok || res.NodeSecret != "" || n.registered {
			t.Errorf("result %+v, registered %v; want a refusal that hands and registers nothing", res, n.registered)
		}
	})

	// And one the enrolment has just stored.
	t.Run("reset pairing right after an enrolment stored its key", func(t *testing.T) {
		k := newKey(t)
		acl := &keyedACL{clusterOK: true, afterStore: resetPairing}
		n := asNode(func() *pb.NodeAuth { a := authFor("eu-node-00"); a.NodePublicKey = pubOf(k); return a }(), nil, k)
		if err := dial(t, rejectingLookup{}, acl, nil, n); err == nil {
			t.Fatal("the enrolment survived the Reset")
		}
		if res := n.result(); res == nil || res.Ok || res.NodeSecret != "" || res.AssignedId != "" || n.registered {
			t.Errorf("result %+v, registered %v; want a refusal that hands and registers nothing", res, n.registered)
		}
	})
}

// No downgrade: once a row holds a key, the secret challenge never logs it in,
// and neither a cluster proof nor an armed admission changes that for a node
// that cannot sign.
func TestAKeyRowRefusesTheSecretChallenge(t *testing.T) {
	k := newKey(t)
	for name, n := range map[string]*fakeNode{
		"an image without keys, answering with its secret":     nodeWith(nil, rowSecret, nil),
		"the right key presented, answered by the secret only": nodeWith(pubOf(k), rowSecret, nil),
	} {
		t.Run(name, func(t *testing.T) {
			acl := &keyedACL{secret: rowSecret, key: pubOf(k), clusterOK: true}
			joins := &recordingJoins{admitIP: "203.0.113.7"}
			if err := dial(t, known, acl, joins, n); err == nil {
				t.Fatal("a key row admitted a node by its secret")
			}
			if res := n.result(); res == nil || res.Ok || res.NodeSecret != "" {
				t.Errorf("auth result = %+v, want a refusal carrying no secret", res)
			}
			if len(joins.recorded) != 1 {
				t.Errorf("recorded %d refusals, want 1", len(joins.recorded))
			}
			if len(acl.stored) != 0 || joins.consumed != 0 {
				t.Errorf("stored %v and consumed %d admission(s) for a refused node", acl.stored, joins.consumed)
			}
		})
	}
}

// A bad signature is a refusal an operator can see, recorded where a wrong
// secret is - including a valid signature made for another node's login.
func TestABadSignatureIsRefusedAndRecorded(t *testing.T) {
	k, impostor := newKey(t), newKey(t)
	for name, n := range map[string]*fakeNode{
		"another key's signature": nodeWith(pubOf(k), rowSecret, impostor),
		"a signature made for another node's login": func() *fakeNode {
			n := nodeWith(pubOf(k), rowSecret, k)
			n.signAs = "node-xyz"
			return n
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			acl := &keyedACL{secret: rowSecret, key: pubOf(k)}
			joins := &recordingJoins{}
			if err := dial(t, known, acl, joins, n); err == nil {
				t.Fatal("the signature was accepted")
			}
			if res := n.result(); res == nil || res.Ok || res.Message != "bad challenge response" {
				t.Errorf("auth result = %+v, want the bad-challenge refusal", res)
			}
			if len(joins.recorded) != 1 || joins.recorded[0].Reason == "" {
				t.Errorf("recorded %+v, want one refusal with a reason", joins.recorded)
			}
			if len(joins.authenticated) != 0 {
				t.Error("a refused node was recorded as authenticated")
			}
		})
	}
}

// Trust on first use: a row with no key registers the key its node presents,
// anchored by the secret challenge the node has just passed - and from then on
// only the key logs it in.
func TestASecretRowRegistersThePresentedKey(t *testing.T) {
	k := newKey(t)
	acl := &keyedACL{secret: rowSecret}
	n := nodeWith(pubOf(k), rowSecret, k)

	if err := dial(t, known, acl, &recordingJoins{}, n); err != nil {
		t.Fatalf("an upgraded node was refused on its secret: %v", err)
	}
	if res := n.result(); res == nil || !res.Ok || res.NodeSecret != "" {
		t.Errorf("auth result = %+v, want ok with no secret, as before keys", res)
	}
	if len(acl.stored) != 1 || !bytes.Equal(acl.stored[0], pubOf(k)) || acl.storedFor[0] != 42 {
		t.Fatalf("stored %v on %v, want the presented key on node 42", acl.stored, acl.storedFor)
	}

	if err := dial(t, known, acl, &recordingJoins{}, nodeWith(nil, rowSecret, nil)); err == nil {
		t.Error("after registering a key, the row still admitted the secret alone")
	}
}

// The secret cannot register a key the node does not hold.
func TestASecretRowDoesNotRegisterAKeyTheNodeCannotSignWith(t *testing.T) {
	k, other := newKey(t), newKey(t)
	acl := &keyedACL{secret: rowSecret}
	if err := dial(t, known, acl, &recordingJoins{}, nodeWith(pubOf(k), rowSecret, other)); err == nil {
		t.Fatal("a key the node could not sign with was accepted")
	}
	if len(acl.stored) != 0 {
		t.Errorf("stored %v", acl.stored)
	}
}

// An image that predates keys, against a row that never had one: exactly as
// before - one challenge, the HMAC decides, nothing stored.
func TestASecretRowWithAnOldImageIsUnchanged(t *testing.T) {
	acl := &keyedACL{secret: rowSecret}
	n := nodeWith(nil, rowSecret, nil)
	if err := dial(t, known, acl, &recordingJoins{}, n); err != nil {
		t.Fatalf("an old image was refused: %v", err)
	}
	if n.challenges != 1 || len(acl.stored) != 0 || n.result().NodeSecret != "" {
		t.Errorf("challenges=%d stored=%v secret=%q, want 1, none, none", n.challenges, acl.stored, n.result().NodeSecret)
	}

	joins := &recordingJoins{}
	wrong := nodeWith(nil, bytes.Repeat([]byte{0x99}, 32), nil)
	if err := dial(t, known, acl, joins, wrong); err == nil {
		t.Fatal("a wrong secret was accepted")
	}
	if len(joins.recorded) != 1 {
		t.Errorf("recorded %d refusals for a wrong secret, want 1", len(joins.recorded))
	}
}

// The presented key goes on the row the enrolment creates, whichever door it
// came through. A key the node cannot sign with spends nothing, and an image
// without keys enrols as it always did.
func TestEnrolmentStoresThePresentedKey(t *testing.T) {
	for _, tc := range []struct {
		name        string
		enrollToken string
		cluster     bool
		wantID      int
	}{
		{name: "cluster proof, a platform node", cluster: true, wantID: 7},
		{name: "enroll token, a BYON node", enrollToken: "a-valid-enroll-token", wantID: 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKey(t)
			a := authFor("eu-node-00")
			a.EnrollToken, a.NodePublicKey = tc.enrollToken, pubOf(k)
			acl := &keyedACL{clusterOK: tc.cluster}
			n := asNode(a, nil, k)
			if err := dial(t, rejectingLookup{}, acl, nil, n); err != nil {
				t.Fatalf("enrolment refused: %v", err)
			}
			if len(acl.stored) != 1 || acl.storedFor[0] != tc.wantID || !bytes.Equal(acl.stored[0], pubOf(k)) {
				t.Errorf("stored %v on %v, want the presented key on node %d", acl.stored, acl.storedFor, tc.wantID)
			}
		})
	}

	t.Run("a key the node cannot sign with spends nothing", func(t *testing.T) {
		a := authFor("eu-node-00")
		a.EnrollToken, a.NodePublicKey = "a-valid-enroll-token", pubOf(newKey(t))
		acl := &keyedACL{clusterOK: true}
		if err := dial(t, rejectingLookup{}, acl, nil, asNode(a, nil, newKey(t))); err == nil {
			t.Fatal("enrolled a node that could not prove its key")
		}
		if acl.enrolls+acl.platformEnrolls != 0 {
			t.Errorf("created %d node(s) before the key was proven", acl.enrolls+acl.platformEnrolls)
		}
	})

	t.Run("an image without keys enrols as before", func(t *testing.T) {
		acl := &keyedACL{clusterOK: true}
		n := asNode(authFor("eu-node-00"), nil, nil)
		if err := dial(t, rejectingLookup{}, acl, nil, n); err != nil {
			t.Fatalf("enrolment refused: %v", err)
		}
		if n.challenges != 0 || len(acl.stored) != 0 {
			t.Errorf("challenges=%d stored=%v, want neither", n.challenges, acl.stored)
		}
	})
}

// The key an operator moved aside is answered with the flag that makes the node
// generate a new one - before any challenge, and without spending the admission
// the operator armed for the NEW key.
func TestARejectedKeyIsToldToGenerateANewOne(t *testing.T) {
	old := newKey(t)
	acl := &keyedACL{secret: rowSecret, rejected: pubOf(old), clusterOK: true}
	joins := &recordingJoins{admitIP: "203.0.113.7"}
	n := nodeWith(pubOf(old), rowSecret, old)

	if err := dial(t, known, acl, joins, n); err == nil {
		t.Fatal("the rejected key was admitted")
	}
	res := n.result()
	if res == nil || res.Ok || !res.NodeKeyRejected {
		t.Fatalf("auth result = %+v, want a refusal flagged node_key_rejected", res)
	}
	if n.challenges != 0 || len(acl.stored) != 0 || joins.consumed != 0 {
		t.Errorf("challenges=%d stored=%v consumed=%d, want none of them", n.challenges, acl.stored, joins.consumed)
	}
}

// A node that lost its cached secret but kept its key is handed the secret Core
// already holds - the same bytes, never a new one, because every Redis password
// it and its containers use derives from it.
func TestAKeyNodeWithoutItsSecretIsHandedTheStoredOne(t *testing.T) {
	k := newKey(t)
	acl := &keyedACL{secret: rowSecret, key: pubOf(k)}
	n := nodeWith(pubOf(k), nil, k)

	if err := dial(t, known, acl, &recordingJoins{}, n); err != nil {
		t.Fatalf("refused: %v", err)
	}
	if got := n.result().NodeSecret; got != hex.EncodeToString(rowSecret) {
		t.Errorf("handed %q, want the stored secret %x", got, rowSecret)
	}
	if !bytes.Equal(acl.secret, rowSecret) {
		t.Error("the stored secret was rotated")
	}
}

// A cluster proof never overrides a live login credential. A node's token is
// close to public, so a cluster proof that replaced a LIVE key would make
// CLUSTER_SECRET alone a login as every keyed platform node - handed its stored
// service secret and its server commands. The refusal is recorded and names the
// remedy, because a platform node that lost its .node_key lands exactly here.
func TestAClusterProofNeverReplacesALiveKey(t *testing.T) {
	old, fresh := newKey(t), newKey(t)
	acl := &keyedACL{secret: rowSecret, key: pubOf(old), clusterOK: true}
	joins := &recordingJoins{}
	n := nodeWith(pubOf(fresh), nil, fresh)
	n.auth.ClusterProof = "a-valid-cluster-proof"

	err := dial(t, known, acl, joins, n)
	res := n.result()
	t.Logf("err=%v ok=%v handed=%q rowKeyNow=%x registered=%v", err, res.GetOk(), res.GetNodeSecret(), acl.key, n.registered)
	if err == nil || res == nil || res.Ok {
		t.Fatal("a cluster proof replaced a live key")
	}
	if res.NodeSecret != "" {
		t.Errorf("handed out %q", res.NodeSecret)
	}
	if len(acl.stored) != 0 || !bytes.Equal(acl.key, pubOf(old)) {
		t.Errorf("stored %v, the row's key is now %x; want the live key untouched", acl.stored, acl.key)
	}
	if n.registered || len(joins.authenticated) != 0 {
		t.Error("the caller was registered")
	}
	if len(joins.recorded) != 1 || !strings.Contains(joins.recorded[0].Reason, "Roll key") {
		t.Errorf("recorded %+v, want one refusal that names Roll key", joins.recorded)
	}
	if !strings.Contains(res.Message, "Roll key") {
		t.Errorf("the node was told %q, want the remedy", res.Message)
	}
}

// Where no live login stands in the way, a platform node's cluster proof
// re-pairs it on the row Core already has: no new row, no new identity. What it
// is handed follows what the operator did - the stored secret after Roll key
// (key aside, secret kept, so nothing restarts), a new one after Reset pairing.
// A row that never had a key but holds a secret still wants the HMAC of it.
func TestAClusterProofRePairsOnlyWithoutALiveLogin(t *testing.T) {
	old, fresh := newKey(t), newKey(t)
	for _, tc := range []struct {
		name       string
		act        func(*keyedACL)
		wantSecret string
	}{
		{"after Roll key, with the stored secret", rollKey, hex.EncodeToString(rowSecret)},
		{"after Reset pairing, with a new secret", resetPairing, hex.EncodeToString(bytes.Repeat([]byte{0x42}, 32))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acl := &keyedACL{secret: rowSecret, key: pubOf(old), clusterOK: true}
			tc.act(acl)
			n := nodeWith(pubOf(fresh), nil, fresh)
			n.auth.ClusterProof = "a-valid-cluster-proof"

			if err := dial(t, known, acl, &recordingJoins{}, n); err != nil {
				t.Fatalf("refused: %v", err)
			}
			res := n.result()
			if acl.platformEnrolls+acl.enrolls != 0 || res.AssignedId != "" {
				t.Errorf("created %d row(s), assigned %q; want the existing row and identity", acl.platformEnrolls+acl.enrolls, res.AssignedId)
			}
			if len(acl.stored) != 1 || acl.storedFor[0] != 42 || !bytes.Equal(acl.key, pubOf(fresh)) {
				t.Errorf("stored %v on %v, want the new key on node 42", acl.stored, acl.storedFor)
			}
			if res.NodeSecret != tc.wantSecret {
				t.Errorf("handed %q, want %q", res.NodeSecret, tc.wantSecret)
			}
			if !n.registered {
				t.Error("the re-paired node was not registered")
			}
		})
	}

	t.Run("a never-keyed row with a secret still needs its HMAC", func(t *testing.T) {
		acl := &keyedACL{secret: rowSecret, clusterOK: true}
		joins := &recordingJoins{}
		n := nodeWith(pubOf(fresh), nil, fresh)
		n.auth.ClusterProof = "a-valid-cluster-proof"

		if err := dial(t, known, acl, joins, n); err == nil {
			t.Fatal("a cluster proof stood in for the row's secret")
		}
		if res := n.result(); res == nil || res.Ok || res.NodeSecret != "" || n.registered {
			t.Errorf("result %+v, registered %v; want a refusal that hands and registers nothing", res, n.registered)
		}
		if len(acl.stored) != 0 || len(joins.recorded) != 1 {
			t.Errorf("stored %v, recorded %d; want nothing stored and the refusal recorded", acl.stored, len(joins.recorded))
		}
	})
}

// The cluster proof is the operator's door and never opens a customer's row. A
// party holding CLUSTER_SECRET and a BYON node's id must not be able to put its
// own key on that row, be handed the node's stored service secret and take its
// server commands, whatever state the row is in. An admission armed in the
// panel still re-pairs it.
func TestAClusterProofNeverRePairsACustomersNode(t *testing.T) {
	legit, attacker := newKey(t), newKey(t)
	for _, tc := range []struct {
		name string
		acl  *keyedACL
	}{
		{"a key row", &keyedACL{secret: rowSecret, key: pubOf(legit), clusterOK: true}},
		{"a key row an operator reset", &keyedACL{secret: rowSecret, rejected: pubOf(legit), clusterOK: true}},
		{"a row whose secret was cleared", &keyedACL{clusterOK: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyBefore := tc.acl.key
			joins := &recordingJoins{}
			n := nodeWith(pubOf(attacker), nil, attacker)
			n.auth.ClusterProof = "a-valid-cluster-proof"

			err := dial(t, customer, tc.acl, joins, n)
			res := n.result()
			t.Logf("err=%v ok=%v handedSecret=%v keyNowAttacker=%v",
				err, res.GetOk(), res.GetNodeSecret() != "", bytes.Equal(tc.acl.key, pubOf(attacker)))
			if err == nil || res == nil || res.Ok {
				t.Fatal("a cluster proof re-paired a customer's node")
			}
			if res.NodeSecret != "" {
				t.Error("the customer node's secret was handed out")
			}
			if len(tc.acl.stored) != 0 || !bytes.Equal(tc.acl.key, keyBefore) {
				t.Errorf("the row's key changed to %x", tc.acl.key)
			}
			if len(joins.recorded) != 1 || len(joins.authenticated) != 0 {
				t.Errorf("recorded %d refusal(s), %d authentication(s); want 1 and 0", len(joins.recorded), len(joins.authenticated))
			}
		})
	}

	t.Run("an admission armed in the panel still re-pairs it", func(t *testing.T) {
		fresh := newKey(t)
		acl := &keyedACL{secret: rowSecret, rejected: pubOf(legit)}
		joins := &recordingJoins{admitIP: "203.0.113.7"}
		n := nodeWith(pubOf(fresh), nil, fresh)
		if err := dial(t, customer, acl, joins, n); err != nil {
			t.Fatalf("refused: %v", err)
		}
		if joins.consumed != 1 || !bytes.Equal(acl.key, pubOf(fresh)) {
			t.Errorf("consumed=%d key=%x, want the admission spent on the new key", joins.consumed, acl.key)
		}
		if n.result().NodeSecret != hex.EncodeToString(rowSecret) {
			t.Error("the re-admitted node was not handed its own secret back")
		}
	})
}

// The state Roll key or an approval leaves a key node in: key moved aside,
// secret kept. The node's new key gets in through the admission, and the kept
// secret is not enough on its own.
func TestAReplacedKeyComesBackOnlyThroughAnAdmission(t *testing.T) {
	old, fresh := newKey(t), newKey(t)

	t.Run("the secret alone is not enough", func(t *testing.T) {
		acl := &keyedACL{secret: rowSecret, rejected: pubOf(old)}
		joins := &recordingJoins{}
		if err := dial(t, known, acl, joins, nodeWith(pubOf(fresh), rowSecret, fresh)); err == nil {
			t.Fatal("a reset key node was admitted by its secret")
		}
		if len(acl.stored) != 0 || len(joins.recorded) != 1 {
			t.Errorf("stored %v, recorded %d", acl.stored, len(joins.recorded))
		}
	})

	t.Run("the admission lets the new key in", func(t *testing.T) {
		acl := &keyedACL{secret: rowSecret, rejected: pubOf(old)}
		joins := &recordingJoins{admitIP: "203.0.113.7"}
		n := nodeWith(pubOf(fresh), rowSecret, fresh)
		if err := dial(t, known, acl, joins, n); err != nil {
			t.Fatalf("refused: %v", err)
		}
		if joins.consumed != 1 || len(acl.stored) != 1 || !bytes.Equal(acl.key, pubOf(fresh)) {
			t.Errorf("consumed=%d stored=%v, want the admission spent on the new key", joins.consumed, acl.stored)
		}
		if n.result().NodeSecret != hex.EncodeToString(rowSecret) {
			t.Error("the re-admitted key node was not handed its own secret back")
		}
	})
}

// A node dials every Core replica at once, so two of them can write this row's
// key at the same moment. The later write never replaces the earlier: losing to
// the same key is a success, losing to a different one is a refusal.
func TestAKeyRaceBetweenReplicas(t *testing.T) {
	old, fresh, other := newKey(t), newKey(t), newKey(t)

	// Both re-pairs start after Roll key (key aside, secret kept): the state a
	// cluster proof re-pairs from.
	t.Run("a re-pair that loses to the same key succeeds", func(t *testing.T) {
		acl := &keyedACL{secret: rowSecret, rejected: pubOf(old), clusterOK: true, raceTo: pubOf(fresh)}
		n := nodeWith(pubOf(fresh), nil, fresh)
		if err := dial(t, known, acl, &recordingJoins{}, n); err != nil {
			t.Fatalf("refused: %v", err)
		}
		if n.result().NodeSecret != hex.EncodeToString(rowSecret) || !bytes.Equal(acl.key, pubOf(fresh)) {
			t.Error("the node was not admitted on the key the other replica stored")
		}
	})
	t.Run("a re-pair that loses to a different key is refused", func(t *testing.T) {
		acl := &keyedACL{secret: rowSecret, rejected: pubOf(old), clusterOK: true, raceTo: pubOf(other)}
		n := nodeWith(pubOf(fresh), nil, fresh)
		if err := dial(t, known, acl, &recordingJoins{}, n); err == nil {
			t.Fatal("admitted on a key the row no longer holds")
		}
		if n.result().NodeSecret != "" || !bytes.Equal(acl.key, pubOf(other)) {
			t.Error("the losing replica handed out the secret or overwrote the winner")
		}
	})
	t.Run("a first key login that loses to a different key is refused", func(t *testing.T) {
		acl := &keyedACL{secret: rowSecret, raceTo: pubOf(other)}
		n := nodeWith(pubOf(fresh), rowSecret, fresh)
		if err := dial(t, known, acl, &recordingJoins{}, n); err == nil {
			t.Fatal("admitted although the row registered another key meanwhile")
		}
		if !bytes.Equal(acl.key, pubOf(other)) {
			t.Error("the losing replica overwrote the winner")
		}
	})
}

// failingLookup is Core failing to KNOW, not Core not having the row.
type failingLookup struct{}

func (failingLookup) GetNodeByToken(string) (*Node, error) {
	return nil, errors.New("db: connection reset")
}

// A node Core may well have must not be enrolled again because a lookup failed.
func TestALookupErrorNeverEnrols(t *testing.T) {
	acl := &keyedACL{clusterOK: true}
	if err := dial(t, failingLookup{}, acl, nil, asNode(authFor("node-abc"), nil, nil)); err == nil {
		t.Fatal("a failed lookup was treated as an unknown node and admitted")
	}
	if acl.platformEnrolls+acl.enrolls != 0 {
		t.Errorf("created %d row(s) on a lookup error", acl.platformEnrolls+acl.enrolls)
	}
}

// A key of the wrong length, or of small order - one a signature can be forged
// for without any private key - is refused before any challenge.
func TestAnUnusableKeyIsRefused(t *testing.T) {
	identityPoint := make([]byte, ed25519.PublicKeySize)
	identityPoint[0] = 1
	for name, key := range map[string][]byte{
		"three bytes":        {1, 2, 3},
		"the identity point": identityPoint,
	} {
		t.Run(name, func(t *testing.T) {
			a := authFor("node-abc")
			a.NodePublicKey = key
			n := asNode(a, nil, nil)
			acl := &keyedACL{secret: rowSecret}
			if err := dial(t, known, acl, nil, n); err == nil {
				t.Fatal("the key was accepted")
			}
			if res := n.result(); res == nil || res.Message != "malformed node key" {
				t.Errorf("auth result = %+v", res)
			}
			if n.challenges != 0 || len(acl.stored) != 0 {
				t.Errorf("challenges=%d stored=%v, want neither", n.challenges, acl.stored)
			}
		})
	}
}
