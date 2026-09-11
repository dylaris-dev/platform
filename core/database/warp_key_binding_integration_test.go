package database

import (
	"errors"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// The binding between a BYON node key and its node is two conditional UPDATEs,
// and every condition in them is something only Postgres checks: the join
// through node_enroll_tokens, the owner on both ends, the partial unique index
// and ON DELETE SET NULL. Built from nothing, because the reference to nodes has
// to exist on a fresh install's first boot, not only on a database that already
// had the tables - and booted twice, because every restart runs it again.
func TestIntegrationWarpKeyBinding(t *testing.T) {
	db := freshSchemaDB(t)
	if err := EnsureSchema(db, false); err != nil {
		t.Fatalf("second boot over the same tables: %v", err)
	}
	st := store.NewPostgresStore(db)

	f := newFixture(t, st)
	if err := st.SetNodeOwner(f.node.ID, &f.user.ID); err != nil {
		t.Fatalf("SetNodeOwner: %v", err)
	}
	other := newFixture(t, st)
	if err := st.SetNodeOwner(other.node.ID, &other.user.ID); err != nil {
		t.Fatalf("SetNodeOwner (other): %v", err)
	}

	newNode := func() *models.Node {
		n := &models.Node{Name: uniqueName("n_"), Address: "127.0.0.1", Token: uniqueName("t_"), Status: "online"}
		if err := st.CreateNode(n); err != nil {
			t.Fatalf("CreateNode: %v", err)
		}
		if err := st.SetNodeOwner(n.ID, &f.user.ID); err != nil {
			t.Fatalf("SetNodeOwner: %v", err)
		}
		return n
	}
	newKey := func(owner string) *store.WarpAPIKey {
		id := uniqueName("node-")
		if _, err := st.CreateWarpAPIKey(store.WarpAPIKey{
			Name: "k", KeyHash: uniqueName("h_"), Policy: "general", MaxConns: 1,
			OnNewConn: "kill_old", NodeID: id, OwnerID: owner,
		}); err != nil {
			t.Fatalf("CreateWarpAPIKey: %v", err)
		}
		k, err := st.GetWarpAPIKeyByNodeID(id)
		if err != nil {
			t.Fatalf("GetWarpAPIKeyByNodeID: %v", err)
		}
		return k
	}
	boundTo := func(k *store.WarpAPIKey) int {
		got, err := st.GetWarpAPIKeyByNodeID(k.NodeID)
		if err != nil {
			t.Fatalf("GetWarpAPIKeyByNodeID: %v", err)
		}
		return got.BoundNodeID
	}
	tokenFor := func(keyNodeID string) string {
		tok := uniqueName("tok_")
		if err := st.CreateNodeEnrollToken(f.user.ID, tok, "l", nil, keyNodeID); err != nil {
			t.Fatalf("CreateNodeEnrollToken: %v", err)
		}
		return tok
	}

	// A token minted without a key binds nothing.
	if bound, err := st.BindWarpKeyFromEnrollToken(tokenFor(""), f.node.ID); err != nil || bound {
		t.Errorf("token without a key: bound=%v err=%v, want false, nil", bound, err)
	}

	// A token minted with the owner's key binds it to the node, exactly once.
	key := newKey(f.user.ID)
	tok := tokenFor(key.NodeID)
	if bound, err := st.BindWarpKeyFromEnrollToken(tok, f.node.ID); err != nil || !bound {
		t.Fatalf("token with a key: bound=%v err=%v, want true, nil", bound, err)
	}
	if got := boundTo(key); got != f.node.ID {
		t.Fatalf("key bound to %d, want %d", got, f.node.ID)
	}
	if bound, err := st.BindWarpKeyFromEnrollToken(tok, f.node.ID); err != nil || bound {
		t.Errorf("a second run: bound=%v err=%v, want false, nil", bound, err)
	}

	// Someone else's key named on this owner's token is never bound.
	spare := newNode()
	foreign := newKey(other.user.ID)
	if bound, err := st.BindWarpKeyFromEnrollToken(tokenFor(foreign.NodeID), spare.ID); err != nil || bound {
		t.Errorf("foreign key: bound=%v err=%v, want false, nil", bound, err)
	}
	if got := boundTo(foreign); got != 0 {
		t.Errorf("foreign key bound to %d", got)
	}

	// Nor is the owner's key bound to someone else's machine.
	free := newKey(f.user.ID)
	if bound, err := st.BindWarpKeyFromEnrollToken(tokenFor(free.NodeID), other.node.ID); err != nil || bound {
		t.Errorf("foreign machine: bound=%v err=%v, want false, nil", bound, err)
	}

	// Nor a revoked key.
	revoked := newKey(f.user.ID)
	if err := st.RevokeWarpAPIKeyByNodeID(revoked.NodeID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if bound, err := st.BindWarpKeyFromEnrollToken(tokenFor(revoked.NodeID), spare.ID); err != nil || bound {
		t.Errorf("revoked key: bound=%v err=%v, want false, nil", bound, err)
	}

	// The bind endpoint's statement. One live key per machine: the index
	// refuses a second one, and the refusal comes back as the sentinel the
	// handler turns into a 409 rather than a raw driver error.
	if _, err := st.BindWarpAPIKey(free.ID, f.node.ID); !errors.Is(err, store.ErrWarpKeyNodeTaken) {
		t.Errorf("a second live key for the same machine: err = %v, want ErrWarpKeyNodeTaken", err)
	}
	// A bound key does not move.
	if bound, err := st.BindWarpAPIKey(key.ID, spare.ID); err != nil || bound {
		t.Errorf("moving a bound key: bound=%v err=%v, want false, nil", bound, err)
	}
	// Revoking the machine's key frees the machine for another.
	if err := st.RevokeWarpAPIKeyByNodeID(key.NodeID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if bound, err := st.BindWarpAPIKey(free.ID, f.node.ID); err != nil || !bound {
		t.Fatalf("after the old key was revoked: bound=%v err=%v, want true, nil", bound, err)
	}
	keys, err := st.ListWarpAPIKeysByOwner(f.user.ID)
	if err != nil {
		t.Fatalf("ListWarpAPIKeysByOwner: %v", err)
	}
	listed := false
	for _, k := range keys {
		if k.NodeID == free.NodeID {
			listed = k.BoundNodeID == f.node.ID
		}
	}
	if !listed {
		t.Error("the owner's key list does not carry the binding")
	}

	// Removing a machine keeps its key row - it still counts and is what the
	// owner revokes - and leaves it unbound.
	gone := newNode()
	last := newKey(f.user.ID)
	if bound, err := st.BindWarpAPIKey(last.ID, gone.ID); err != nil || !bound {
		t.Fatalf("bind before delete: bound=%v err=%v", bound, err)
	}
	if err := st.DeleteNode(gone.ID); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	if got := boundTo(last); got != 0 {
		t.Errorf("key still bound to deleted node %d", got)
	}
}
