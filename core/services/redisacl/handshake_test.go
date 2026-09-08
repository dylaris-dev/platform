package redisacl

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fakeHandshakeStore is a minimal in-memory HandshakeStore. Every branch is
// driven by explicit fields rather than a real DB, so each test only sets what
// it needs. Call counters let tests assert a step was (or was NOT) reached -
// e.g. that Enroll never consumes the token when the node limit is hit.
type fakeHandshakeStore struct {
	secretEnc   map[int]string
	uuidsByNode map[int][]string

	resolveOwnerID string
	resolveOK      bool
	resolveErr     error
	resolveCalls   int

	consumeOwnerID string
	consumeOK      bool
	consumeErr     error
	consumeCalls   int

	nodeLimitReached bool

	createNodeID                                                                 int
	createErr                                                                    error
	createCalls                                                                  int
	lastCreateToken, lastCreateAddress, lastCreateOwnerID, lastCreateDisplayName string

	nodeIDByTokenID    int
	nodeIDByTokenFound bool
	nodeIDByTokenErr   error
}

func newFakeHandshakeStore() *fakeHandshakeStore {
	return &fakeHandshakeStore{secretEnc: map[int]string{}, uuidsByNode: map[int][]string{}}
}

func (f *fakeHandshakeStore) GetNodeSecretEnc(id int) (string, error) { return f.secretEnc[id], nil }
func (f *fakeHandshakeStore) SetNodeSecretEnc(id int, enc string) error {
	f.secretEnc[id] = enc
	return nil
}
func (f *fakeHandshakeStore) SetNodeSecretEncIfUnchanged(id int, prev, next string) (bool, error) {
	if f.secretEnc[id] != prev {
		return false, nil
	}
	f.secretEnc[id] = next
	return true, nil
}
func (f *fakeHandshakeStore) ServerUUIDsByNode(nodeID int) ([]string, error) {
	return f.uuidsByNode[nodeID], nil
}
func (f *fakeHandshakeStore) ResolveEnrollToken(plaintext string) (string, bool, error) {
	f.resolveCalls++
	return f.resolveOwnerID, f.resolveOK, f.resolveErr
}
func (f *fakeHandshakeStore) ConsumeEnrollToken(plaintext string) (string, bool, error) {
	f.consumeCalls++
	return f.consumeOwnerID, f.consumeOK, f.consumeErr
}
func (f *fakeHandshakeStore) NodeLimitReached(ownerID string) bool { return f.nodeLimitReached }
func (f *fakeHandshakeStore) CreateBYONNode(token, address, ownerID, displayName string) (int, error) {
	f.createCalls++
	f.lastCreateToken, f.lastCreateAddress, f.lastCreateOwnerID, f.lastCreateDisplayName = token, address, ownerID, displayName
	return f.createNodeID, f.createErr
}
func (f *fakeHandshakeStore) CreatePlatformNode(token, address, displayName string) (int, error) {
	f.createCalls++
	f.lastCreateToken, f.lastCreateAddress, f.lastCreateDisplayName = token, address, displayName
	f.lastCreateOwnerID = "" // a platform node has no owner
	return f.createNodeID, f.createErr
}
func (f *fakeHandshakeStore) NodeIDByToken(token string) (int, bool, error) {
	return f.nodeIDByTokenID, f.nodeIDByTokenFound, f.nodeIDByTokenErr
}

// newTestProvisioner points a Provisioner at miniredis. miniredis does NOT
// implement the ACL command family (verified directly: ACL SETUSER / ACL SAVE
// both return "ERR unknown command `ACL`"), so any Handshake path that reaches
// Provisioner.EnsureNodeACL will fail here. That is intentional: those tests
// assert the decision logic reached the provisioning step and that the error
// surfaces (or is swallowed, per the caller's contract) - not the ACL
// application itself, which needs real Redis (integration scope, out of unit
// scope here).
func newTestProvisioner(t *testing.T) *Provisioner {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewProvisioner(rdb)
}

func TestEnsureForToken_UnknownToken_IsNoOp(t *testing.T) {
	store := newFakeHandshakeStore()
	store.nodeIDByTokenFound = false
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	if err := h.EnsureForToken(context.Background(), "unknown-token"); err != nil {
		t.Fatalf("EnsureForToken for an unknown token must be a no-op (nil), got: %v", err)
	}
	if len(store.secretEnc) != 0 {
		t.Error("no secret should be minted for an unknown token")
	}
}

func TestEnsureForToken_StoreLookupError_Propagates(t *testing.T) {
	store := newFakeHandshakeStore()
	wantErr := errNodeLookup("boom")
	store.nodeIDByTokenErr = wantErr
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	if err := h.EnsureForToken(context.Background(), "tok"); err != wantErr {
		t.Fatalf("EnsureForToken = %v, want %v", err, wantErr)
	}
}

func TestEnsureForToken_KnownToken_ReachesProvisioningAndPropagatesError(t *testing.T) {
	store := newFakeHandshakeStore()
	store.nodeIDByTokenID = 5
	store.nodeIDByTokenFound = true
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	// Known token: EnsureForToken must attempt full provisioning (mint/load
	// secret, then apply the ACL). Since miniredis rejects ACL SETUSER, the
	// error must surface here (this method does not swallow it - only the
	// caller in services.QueueService.SendCommand does, at a higher layer).
	err := h.EnsureForToken(context.Background(), "tok-known")
	if err == nil {
		t.Fatal("expected the ACL provisioning error to propagate (miniredis has no ACL support)")
	}
	// The secret must still have been minted before the ACL step failed.
	if _, ok := store.secretEnc[5]; !ok {
		t.Error("expected a secret to be minted for the known node before ACL provisioning was attempted")
	}
}

type errNodeLookup string

func (e errNodeLookup) Error() string { return string(e) }

func TestEnroll_InvalidToken_NoSideEffects(t *testing.T) {
	store := newFakeHandshakeStore()
	store.resolveOK = false
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	_, _, _, err := h.Enroll(context.Background(), "hostname", "bad-enroll-token", "1.2.3.4")
	if err != ErrEnrollInvalid {
		t.Fatalf("err = %v, want ErrEnrollInvalid", err)
	}
	if store.consumeCalls != 0 {
		t.Error("an invalid enroll token must never be consumed")
	}
	if store.createCalls != 0 {
		t.Error("an invalid enroll token must never create a node")
	}
}

func TestEnroll_ResolveError_PropagatesRawError(t *testing.T) {
	store := newFakeHandshakeStore()
	wantErr := errNodeLookup("db down")
	store.resolveErr = wantErr
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	_, _, _, err := h.Enroll(context.Background(), "hostname", "tok", "1.2.3.4")
	if err != wantErr {
		t.Fatalf("err = %v, want the raw resolve error %v (not ErrEnrollInvalid)", err, wantErr)
	}
}

func TestEnroll_NodeLimitReached_DoesNotConsumeToken(t *testing.T) {
	store := newFakeHandshakeStore()
	store.resolveOK = true
	store.resolveOwnerID = "owner-1"
	store.nodeLimitReached = true
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	_, _, _, err := h.Enroll(context.Background(), "hostname", "tok", "1.2.3.4")
	if err != ErrNodeLimit {
		t.Fatalf("err = %v, want ErrNodeLimit", err)
	}
	// The limit check must happen BEFORE the single-use token is consumed, so a
	// rejected enrollment leaves the token intact for a later, valid attempt.
	if store.consumeCalls != 0 {
		t.Error("hitting the node limit must not consume the enroll token")
	}
}

func TestEnroll_ConcurrentConsumeRace_ReturnsInvalid(t *testing.T) {
	store := newFakeHandshakeStore()
	store.resolveOK = true
	store.resolveOwnerID = "owner-1"
	store.nodeLimitReached = false
	// Simulate: another concurrent connect (or the discovery path) already
	// consumed this single-use token between Resolve and Consume.
	store.consumeOK = false
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	_, _, _, err := h.Enroll(context.Background(), "hostname", "tok", "1.2.3.4")
	if err != ErrEnrollInvalid {
		t.Fatalf("err = %v, want ErrEnrollInvalid on a lost consume race", err)
	}
	if store.createCalls != 0 {
		t.Error("a lost consume race must never create a node")
	}
}

func TestEnroll_ConsumeError_Propagates(t *testing.T) {
	store := newFakeHandshakeStore()
	store.resolveOK = true
	wantErr := errNodeLookup("redis down")
	store.consumeErr = wantErr
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	_, _, _, err := h.Enroll(context.Background(), "hostname", "tok", "1.2.3.4")
	if err != wantErr {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestEnroll_CreateBYONNodeError_Propagates(t *testing.T) {
	store := newFakeHandshakeStore()
	store.resolveOK = true
	store.resolveOwnerID = "owner-1"
	store.consumeOK = true
	store.consumeOwnerID = "owner-1"
	wantErr := errNodeLookup("insert failed")
	store.createErr = wantErr
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	_, _, _, err := h.Enroll(context.Background(), "my-hostname", "tok", "1.2.3.4")
	if err != wantErr {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestEnroll_HappyPath_UsesConsumedOwnerAndReachesProvisioning(t *testing.T) {
	store := newFakeHandshakeStore()
	store.resolveOK = true
	store.resolveOwnerID = "owner-from-resolve"
	// The consumed owner (not the resolved one) is what Enroll must bind the
	// node to - they could differ under a race, and Consume is the atomic
	// source of truth.
	store.consumeOK = true
	store.consumeOwnerID = "owner-from-consume"
	store.createNodeID = 42
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	assignedID, nodeID, _, err := h.Enroll(context.Background(), "my-hostname", "enroll-tok", "1.2.3.4")

	if store.createCalls != 1 {
		t.Fatalf("CreateBYONNode calls = %d, want 1", store.createCalls)
	}
	if store.lastCreateOwnerID != "owner-from-consume" {
		t.Errorf("CreateBYONNode owner = %q, want owner-from-consume (the CONSUMED owner)", store.lastCreateOwnerID)
	}
	if store.lastCreateAddress != "1.2.3.4" {
		t.Errorf("CreateBYONNode address = %q, want 1.2.3.4", store.lastCreateAddress)
	}
	if store.lastCreateDisplayName != "my-hostname" {
		t.Errorf("CreateBYONNode displayName = %q, want my-hostname (node-supplied token is kept only as display name)", store.lastCreateDisplayName)
	}
	if store.lastCreateToken == "" || store.lastCreateToken == "my-hostname" {
		t.Errorf("CreateBYONNode assigned token = %q, want a Core-minted UUID, not the raw hostname", store.lastCreateToken)
	}
	if assignedID == "" || assignedID != store.lastCreateToken {
		t.Errorf("returned assignedID = %q, want it to equal the minted token %q", assignedID, store.lastCreateToken)
	}
	if nodeID != 42 {
		t.Errorf("returned nodeID = %d, want 42", nodeID)
	}
	// Provisioning is attempted (and fails against miniredis's missing ACL
	// support): this proves Enroll reaches the ensure() step on the happy path,
	// and that its error is NOT swallowed - the caller must be able to detect a
	// failed enrollment despite the node row having already been created.
	if err == nil {
		t.Fatal("expected the ACL provisioning error to propagate on the happy path (miniredis has no ACL support)")
	}
}

func TestHasSecret(t *testing.T) {
	store := newFakeHandshakeStore()
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	got, err := h.HasSecret(context.Background(), 1)
	if err != nil {
		t.Fatalf("HasSecret: %v", err)
	}
	if got {
		t.Fatal("HasSecret must be false before any secret is minted")
	}

	if _, err := LoadOrCreateNodeSecret(store, "cluster-secret", 1); err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, err = h.HasSecret(context.Background(), 1)
	if err != nil {
		t.Fatalf("HasSecret: %v", err)
	}
	if !got {
		t.Fatal("HasSecret must be true once a secret has been minted")
	}
}

func TestVerifyProof(t *testing.T) {
	store := newFakeHandshakeStore()
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")
	const nodeID = 7
	const token = "node-tok"

	// No secret minted yet: must fail closed without erroring.
	ok, err := h.VerifyProof(context.Background(), nodeID, token, "whatever")
	if err != nil || ok {
		t.Fatalf("VerifyProof with no stored secret = (%v, %v), want (false, nil)", ok, err)
	}

	secret, err := LoadOrCreateNodeSecret(store, "cluster-secret", nodeID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	validProof := Proof(secret, token)

	ok, err = h.VerifyProof(context.Background(), nodeID, token, validProof)
	if err != nil || !ok {
		t.Fatalf("VerifyProof with a valid proof = (%v, %v), want (true, nil)", ok, err)
	}

	ok, err = h.VerifyProof(context.Background(), nodeID, token, "wrong-proof")
	if err != nil || ok {
		t.Fatalf("VerifyProof with an invalid proof = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestVerifyChallenge(t *testing.T) {
	store := newFakeHandshakeStore()
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")
	const nodeID = 8
	const nonce = "nonce-123"

	ok, err := h.VerifyChallenge(context.Background(), nodeID, nonce, "whatever")
	if err != nil || ok {
		t.Fatalf("VerifyChallenge with no stored secret = (%v, %v), want (false, nil)", ok, err)
	}

	secret, err := LoadOrCreateNodeSecret(store, "cluster-secret", nodeID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	validResp := ChallengeResponse(secret, nonce)

	ok, err = h.VerifyChallenge(context.Background(), nodeID, nonce, validResp)
	if err != nil || !ok {
		t.Fatalf("VerifyChallenge with a valid response = (%v, %v), want (true, nil)", ok, err)
	}

	ok, err = h.VerifyChallenge(context.Background(), nodeID, "different-nonce", validResp)
	if err != nil || ok {
		t.Fatalf("VerifyChallenge under a different nonce = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestVerifyClusterProof(t *testing.T) {
	store := newFakeHandshakeStore()
	h := NewHandshake(store, newTestProvisioner(t), "the-cluster-secret")
	const token = "node-tok"

	validProof := ClusterProof("the-cluster-secret", token)
	if !h.VerifyClusterProof(token, validProof) {
		t.Fatal("VerifyClusterProof must accept a proof built from the correct cluster secret")
	}
	if h.VerifyClusterProof(token, "bad-proof") {
		t.Fatal("VerifyClusterProof must reject a wrong proof")
	}
	if h.VerifyClusterProof(token, "") {
		t.Fatal("VerifyClusterProof must reject an empty proof")
	}
	wrongSecretProof := ClusterProof("a-different-cluster-secret", token)
	if h.VerifyClusterProof(token, wrongSecretProof) {
		t.Fatal("VerifyClusterProof must reject a proof built from a different cluster secret")
	}
}
