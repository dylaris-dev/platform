package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

type nodeDeleteFakeStore struct {
	store.Store
	node      *models.Node
	deleted   int
	boundKeys []int
	revoked   []int
}

func (f *nodeDeleteFakeStore) ListWarpKeyIDsBoundToNode(nodeID int) ([]int, error) {
	return f.boundKeys, nil
}

func (f *nodeDeleteFakeStore) RevokeWarpAPIKeyByID(id int) error {
	f.revoked = append(f.revoked, id)
	return nil
}

type recordingPeerDisconnector struct{ keys []int }

func (d *recordingPeerDisconnector) DisconnectKeyPeers(_ context.Context, keyID int) int {
	d.keys = append(d.keys, keyID)
	return 1
}

// Removing a machine used to leave its overlay key live and its WireGuard peer
// connected: the machine stayed on our overlay indefinitely, could re-enroll at
// will, and its Link kept its edge tunnels for up to a day. The key is read
// before the row goes because the delete nulls the binding.
func TestDeleteNode_ReleasesTheMachinesOverlayKeyAndLink(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	owner := "tenant-1"
	for _, c := range []struct {
		name     string
		node     *models.Node
		wantLink bool // true: the link token must survive
	}{
		{"a customer's machine", &models.Node{ID: 7, Token: "t7", LinkToken: "lt7", OwnerID: &owner}, false},
		{"an in-cluster node keeps a Link it may share", &models.Node{ID: 8, Token: "t8", LinkToken: "lt8"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tok := c.node.LinkToken
			rdb.Set(ctx, "link:"+tok, "valid", 0)
			rdb.Set(ctx, "online_link:"+tok, "1", 0)
			fs := &nodeDeleteFakeStore{node: c.node, boundKeys: []int{41}}
			peers := &recordingPeerDisconnector{}
			h := &NodeHandler{state: &AppState{Store: fs, Redis: rdb, WarpPeers: peers}}

			id := strconv.Itoa(c.node.ID)
			rw := httptest.NewRecorder()
			h.DeleteNode(rw, mux.SetURLVars(httptest.NewRequest(http.MethodDelete, "/api/nodes/"+id, nil), map[string]string{"id": id}))
			if rw.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rw.Code, rw.Body.String())
			}
			if len(fs.revoked) != 1 || fs.revoked[0] != 41 {
				t.Errorf("revoked = %v, want the key bound to the machine", fs.revoked)
			}
			if len(peers.keys) != 1 || peers.keys[0] != 41 {
				t.Errorf("peers disconnected for %v, want key 41", peers.keys)
			}
			n, _ := rdb.Exists(ctx, "link:"+tok, "online_link:"+tok).Result()
			if c.wantLink && n != 2 {
				t.Errorf("the Link token of an in-cluster node was dropped")
			}
			if !c.wantLink && n != 0 {
				t.Errorf("the Link token of a removed machine outlived it")
			}
		})
	}
}

func (f *nodeDeleteFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.node == nil || f.node.ID != id {
		return nil, nil
	}
	return f.node, nil
}

func (f *nodeDeleteFakeStore) DeleteNode(id int) error {
	f.deleted = id
	return nil
}

// TestDeleteNode_RemovesTheNodesRedisState covers what deleting a node used to
// leave running.
//
// DeleteNode dropped the DB row and nothing else. The scoped Redis ACL users
// stayed live, and a node caches its secret in dylaris_data on purpose (it
// survives a container recreate), so removing a node from the panel did not
// revoke its Redis access - it kept authenticating with the credential it
// already held. RemoveNodeACL existed for exactly this ("e.g. node deleted" in
// its own doc) and was only wired into the pairing-reset path.
//
// The per-node keys are the housekeeping half: none of them expire and nothing
// else deletes them, so every deleted node left a few behind permanently.
//
// What this test can observe is the keys. The ACL call runs first inside the
// same function, so keys being gone means the function ran through it -
// miniredis implements no ACL command, which is why the DELUSER itself cannot
// be asserted here rather than because it is unimportant.
func TestDeleteNode_RemovesTheNodesRedisState(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	const token = "node-token-abc"
	keys := []string{
		storagePlacementKey(token),
		"dylaris:node:" + token + ":cpu",
		"dylaris:node:" + token + ":cpu:sig",
		"dylaris:node:" + token + ":cmds",
	}
	for _, k := range keys {
		if err := rdb.Set(ctx, k, "x", 0).Err(); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	// A key belonging to a DIFFERENT node must survive: the cleanup is scoped.
	other := "dylaris:node:other-token:cpu"
	rdb.Set(ctx, other, "x", 0)

	fs := &nodeDeleteFakeStore{node: &models.Node{ID: 7, Token: token}}
	h := &NodeHandler{state: &AppState{Store: fs, Redis: rdb}}

	rw := httptest.NewRecorder()
	req := mux.SetURLVars(httptest.NewRequest(http.MethodDelete, "/api/nodes/7", nil), map[string]string{"id": "7"})
	h.DeleteNode(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rw.Code, rw.Body.String())
	}
	if fs.deleted != 7 {
		t.Fatalf("DeleteNode was called with %d, want 7", fs.deleted)
	}
	for _, k := range keys {
		if n, _ := rdb.Exists(ctx, k).Result(); n != 0 {
			t.Errorf("key %q outlived the node", k)
		}
	}
	if n, _ := rdb.Exists(ctx, other).Result(); n != 1 {
		t.Errorf("cleanup removed %q, which belongs to a different node", other)
	}
}

// TestDeleteNode_StillSucceedsWhenTheNodeCannotBeRead is the control: the row
// deletion is what the caller asked for, so an unreadable node must not turn a
// completed delete into an error. It is logged instead.
func TestDeleteNode_StillSucceedsWhenTheNodeCannotBeRead(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	fs := &nodeDeleteFakeStore{node: nil} // GetNodeByID returns (nil, nil)
	h := &NodeHandler{state: &AppState{Store: fs, Redis: rdb}}

	rw := httptest.NewRecorder()
	req := mux.SetURLVars(httptest.NewRequest(http.MethodDelete, "/api/nodes/9", nil), map[string]string{"id": "9"})
	h.DeleteNode(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if fs.deleted != 9 {
		t.Errorf("the row was not deleted (got id %d)", fs.deleted)
	}
}
