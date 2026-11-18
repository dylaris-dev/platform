package redisacl

import (
	"context"
	"testing"
)

// EnrollPlatform is the door for the operator's OWN nodes: a node that already
// holds CLUSTER_SECRET self-enrolls without an enroll token. The invariants that
// must not drift are that it creates an UNOWNED node and never touches the
// enroll-token machinery, so BYON ownership stays impossible to obtain this way.

func TestEnrollPlatform_CreatesUnownedNodeWithMintedIdentity(t *testing.T) {
	store := newFakeHandshakeStore()
	store.createNodeID = 7
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	assignedID, nodeID, _, _ := h.EnrollPlatform(context.Background(), "my-hostname", "1.2.3.4")

	if store.createCalls != 1 {
		t.Fatalf("CreatePlatformNode calls = %d, want 1", store.createCalls)
	}
	if store.lastCreateOwnerID != "" {
		t.Errorf("owner = %q, want empty - a platform node must stay unowned", store.lastCreateOwnerID)
	}
	if store.lastCreateAddress != "1.2.3.4" {
		t.Errorf("address = %q, want 1.2.3.4", store.lastCreateAddress)
	}
	if store.lastCreateDisplayName != "my-hostname" {
		t.Errorf("displayName = %q, want my-hostname (the node-supplied token is cosmetic)", store.lastCreateDisplayName)
	}
	// The identity must be Core-minted and unguessable, exactly as for BYON -
	// otherwise a node could claim an identity by choosing its hostname.
	if store.lastCreateToken == "" || store.lastCreateToken == "my-hostname" {
		t.Errorf("assigned token = %q, want a Core-minted UUID, not the raw hostname", store.lastCreateToken)
	}
	if assignedID != store.lastCreateToken {
		t.Errorf("returned assignedID = %q, want the created token %q", assignedID, store.lastCreateToken)
	}
	if nodeID != 7 {
		t.Errorf("nodeID = %d, want 7", nodeID)
	}
}

func TestEnrollPlatform_NeverTouchesEnrollTokens(t *testing.T) {
	store := newFakeHandshakeStore()
	// Make the token machinery loudly available; EnrollPlatform must still not
	// use it. A platform node has no owner to resolve and no token to burn.
	store.resolveOK, store.resolveOwnerID = true, "owner-1"
	store.consumeOK, store.consumeOwnerID = true, "owner-1"
	store.nodeLimitReached = true // a BYON plan limit must not apply here either
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	if _, _, _, err := h.EnrollPlatform(context.Background(), "host", "1.2.3.4"); err == ErrNodeLimit {
		t.Fatal("EnrollPlatform hit the per-owner node limit; that limit is a BYON billing plan and has no owner to bill here")
	}
	if store.resolveCalls != 0 {
		t.Errorf("ResolveEnrollToken calls = %d, want 0", store.resolveCalls)
	}
	if store.consumeCalls != 0 {
		t.Errorf("ConsumeEnrollToken calls = %d, want 0", store.consumeCalls)
	}
}

func TestEnrollPlatform_CreateError_Propagates(t *testing.T) {
	store := newFakeHandshakeStore()
	wantErr := errNodeLookup("insert failed")
	store.createErr = wantErr
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	if _, _, _, err := h.EnrollPlatform(context.Background(), "host", "1.2.3.4"); err != wantErr {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// Each enrollment must mint a fresh identity; reusing one would let a second
// node take over the first one's servers.
func TestEnrollPlatform_MintsADistinctIdentityEachTime(t *testing.T) {
	store := newFakeHandshakeStore()
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	first, _, _, _ := h.EnrollPlatform(context.Background(), "host", "1.2.3.4")
	second, _, _, _ := h.EnrollPlatform(context.Background(), "host", "1.2.3.4")

	if first == "" || second == "" {
		t.Fatalf("assigned ids must be non-empty, got %q and %q", first, second)
	}
	if first == second {
		t.Errorf("both enrollments got assigned id %q, want distinct identities", first)
	}
}

// A cluster-proof node retries every ~30s when its enrolment is refused, and
// each retry creates a new row. An ACL error after the row exists must not
// refuse it (miniredis has no ACL support, so provisioning fails here).
func TestEnrollPlatform_ACLErrorStillEnrols(t *testing.T) {
	store := newFakeHandshakeStore()
	store.createNodeID = 7
	h := NewHandshake(store, newTestProvisioner(t), "cluster-secret")

	_, _, secretHex, err := h.EnrollPlatform(context.Background(), "host", "1.2.3.4")
	if err != nil {
		t.Fatalf("EnrollPlatform = %v, want the identity despite the ACL error", err)
	}
	assertSecretStored(t, store, 7, secretHex)
}
