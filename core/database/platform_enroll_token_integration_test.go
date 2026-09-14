package database

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// A platform token is an admin's External node token. Three things about it are
// Postgres predicates and nothing else: it reads back as platform, it is not one
// of the minting admin's own pending machines, and it is redeemable only while
// the owner-less key minted with it is live - which is how revoking or purging
// that key in the admin Warp list kills a token nobody has used yet.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationPlatformEnrollToken(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	newKey := func(owner string) *store.WarpAPIKey {
		id := uniqueName("node-")
		keyID, err := st.CreateWarpAPIKey(store.WarpAPIKey{
			Name: "ext", KeyHash: uniqueName("h_"), Policy: "general", MaxConns: 1,
			OnNewConn: "kill_old", NodeID: id, OwnerID: owner,
		})
		if err != nil {
			t.Fatalf("CreateWarpAPIKey: %v", err)
		}
		return &store.WarpAPIKey{ID: keyID, NodeID: id}
	}
	mint := func(keyNodeID string) string {
		tok := uniqueName("ptok_")
		future := time.Now().Add(24 * time.Hour)
		if err := st.CreatePlatformNodeEnrollToken(f.user.ID, tok, "ext", &future, keyNodeID); err != nil {
			t.Fatalf("CreatePlatformNodeEnrollToken: %v", err)
		}
		return tok
	}

	// A tenant token beside it, so the exclusions are shown to exclude only
	// platform tokens.
	if err := st.CreateNodeEnrollToken(f.user.ID, uniqueName("tok_"), "mine", nil, ""); err != nil {
		t.Fatalf("CreateNodeEnrollToken: %v", err)
	}
	live := newKey("")
	liveTok := mint(live.NodeID)

	userID, platform, ok, err := st.ResolveNodeEnrollToken(liveTok)
	if err != nil || !ok || !platform || userID != f.user.ID {
		t.Fatalf("resolve = (%q, platform=%v, ok=%v, %v), want the minter, platform, ok", userID, platform, ok, err)
	}
	if n, err := st.CountPendingNodeEnrollTokens(f.user.ID); err != nil || n != 1 {
		t.Errorf("pending count = (%d, %v), want 1: a platform token is not the admin's own machine", n, err)
	}
	toks, err := st.ListNodeEnrollTokens(f.user.ID)
	if err != nil || len(toks) != 1 || toks[0].Label != "mine" {
		t.Errorf("list = (%+v, %v), want only the tenant token", toks, err)
	}

	// Revoked key: the token cannot be spent.
	revoked := newKey("")
	revokedTok := mint(revoked.NodeID)
	if err := st.RevokeWarpAPIKeyByID(revoked.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, ok, err := st.ConsumeNodeEnrollToken(revokedTok); err != nil || ok {
		t.Errorf("token of a revoked key: ok=%v err=%v, want refused", ok, err)
	}

	// Purged key: the same.
	purged := newKey("")
	purgedTok := mint(purged.NodeID)
	if err := st.DeleteWarpAPIKeyByID(purged.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, ok, err := st.ConsumeNodeEnrollToken(purgedTok); err != nil || ok {
		t.Errorf("token of a purged key: ok=%v err=%v, want refused", ok, err)
	}

	// A platform token naming a TENANT's key is not redeemable: the key it
	// depends on has to be owner-less.
	tenantKey := newKey(f.user.ID)
	if _, _, ok, err := st.ConsumeNodeEnrollToken(mint(tenantKey.NodeID)); err != nil || ok {
		t.Errorf("platform token naming a tenant's key: ok=%v err=%v, want refused", ok, err)
	}

	// The live one is spent exactly once.
	if gotUser, recovers, ok, err := st.ConsumeNodeEnrollToken(liveTok); err != nil || !ok || gotUser != f.user.ID || recovers != "" {
		t.Fatalf("consume the live platform token = (%q, %q, %v, %v), want the minter, ok", gotUser, recovers, ok, err)
	}
	if _, _, ok, err := st.ConsumeNodeEnrollToken(liveTok); err != nil || ok {
		t.Errorf("a second consume: ok=%v err=%v, want refused", ok, err)
	}

	// A tenant token with no key stays redeemable: the key condition is for
	// platform tokens only.
	plain := uniqueName("tok_")
	if err := st.CreateNodeEnrollToken(f.user.ID, plain, "plain", nil, ""); err != nil {
		t.Fatalf("CreateNodeEnrollToken: %v", err)
	}
	if _, platform, ok, err := st.ResolveNodeEnrollToken(plain); err != nil || !ok || platform {
		t.Errorf("tenant token resolve: platform=%v ok=%v err=%v, want not platform, ok", platform, ok, err)
	}
	if _, _, ok, err := st.ConsumeNodeEnrollToken(plain); err != nil || !ok {
		t.Errorf("tenant token without a key: ok=%v err=%v, want consumed", ok, err)
	}
}

// The node login decides whether a cluster proof may re-pair a row from two
// facts it reads through NodeLoginFacts, and the External node's half of that
// is a marker CreatePlatformTokenNode writes. Both ends are pinned here against
// the real columns: the External row reads as unowned AND platform-token, and
// the three rows that keep the cluster-proof door - a cluster-minted one, a
// legacy unmarked one and (for contrast) a customer's - do not read as one.
func TestIntegrationPlatformTokenNodeLoginFacts(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	ext, err := st.CreatePlatformTokenNode(uniqueName("ext_t_"), "10.0.0.9", "ext-box")
	if err != nil {
		t.Fatalf("CreatePlatformTokenNode: %v", err)
	}
	t.Cleanup(func() { st.DeleteNode(ext) })
	extNode, err := st.GetNodeByID(ext)
	if err != nil {
		t.Fatalf("GetNodeByID: %v", err)
	}
	if extNode.OwnerID != nil {
		t.Errorf("External node owner = %q, want none", *extNode.OwnerID)
	}

	mk := func(prefix, via string, owner *string) string {
		n := &models.Node{Name: uniqueName(prefix), Address: "127.0.0.1", Token: uniqueName(prefix + "t_"), Status: "offline"}
		if err := st.CreateNode(n); err != nil {
			t.Fatalf("CreateNode: %v", err)
		}
		t.Cleanup(func() { st.DeleteNode(n.ID) })
		if via != "" {
			if err := st.SetNodeEnrolledVia(n.ID, via); err != nil {
				t.Fatalf("SetNodeEnrolledVia: %v", err)
			}
		}
		if owner != nil {
			if err := st.SetNodeOwner(n.ID, owner); err != nil {
				t.Fatalf("SetNodeOwner: %v", err)
			}
		}
		return n.Token
	}

	tests := []struct {
		name                    string
		token                   string
		wantOwned, wantPlatform bool
	}{
		{"External node", extNode.Token, false, true},
		{"cluster-minted platform node", mk("cp_", store.NodeEnrolledViaClusterProof, nil), false, false},
		{"legacy unmarked platform node", mk("legacy_", "", nil), false, false},
		{"customer's node", mk("byon_", "", &f.user.ID), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, owned, platform, err := st.NodeLoginFacts(tt.token)
			if err != nil || owned != tt.wantOwned || platform != tt.wantPlatform {
				t.Errorf("NodeLoginFacts = (owned=%v, platform=%v, %v), want (owned=%v, platform=%v)",
					owned, platform, err, tt.wantOwned, tt.wantPlatform)
			}
		})
	}

	if _, _, _, err := st.NodeLoginFacts(uniqueName("nobody_")); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("unknown token: err = %v, want sql.ErrNoRows", err)
	}
}
