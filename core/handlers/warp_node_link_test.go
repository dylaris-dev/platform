package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
)

const nodeLinkClusterSecret = "test-cluster-secret"

// nodeLinkFakeStore backs LinkBoot's BYON branch and the bind endpoint: node
// keys by identity, nodes with their encrypted secrets, and a record of every
// node lookup and bind, so a test can assert that a path was NOT taken as well
// as that one was. Embeds store.Store (nil); any other call panics.
type nodeLinkFakeStore struct {
	store.Store
	settings map[string]string
	billing  *store.UserBilling
	keys     map[string]*store.WarpAPIKey
	nodes    map[int]*models.Node
	secrets  map[int]string

	nodeLookups int
	binds       [][2]int
	bindErr     error
}

func (f *nodeLinkFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *nodeLinkFakeStore) GetUserBilling(string) (*store.UserBilling, error) {
	return f.billing, nil
}
func (f *nodeLinkFakeStore) GetWarpAPIKeyByNodeID(id string) (*store.WarpAPIKey, error) {
	if k, ok := f.keys[id]; ok {
		cp := *k
		return &cp, nil
	}
	return nil, warpErr("not found")
}
func (f *nodeLinkFakeStore) ListWarpAPIKeysByOwner(owner string) ([]store.WarpAPIKey, error) {
	var out []store.WarpAPIKey
	for _, k := range f.keys {
		if k.OwnerID == owner && k.RevokedAt == nil {
			out = append(out, *k)
		}
	}
	return out, nil
}
func (f *nodeLinkFakeStore) GetNodeByID(id int) (*models.Node, error) {
	f.nodeLookups++
	if n, ok := f.nodes[id]; ok {
		return n, nil
	}
	return nil, warpErr("no such node")
}
func (f *nodeLinkFakeStore) GetNodeSecretEnc(id int) (string, error) { return f.secrets[id], nil }
func (f *nodeLinkFakeStore) SetNodeSecretEncIfUnchanged(id int, prev, next string) (bool, error) {
	if f.secrets[id] != prev {
		return false, nil
	}
	f.secrets[id] = next
	return true, nil
}
func (f *nodeLinkFakeStore) BindWarpAPIKey(keyID, nodeID int) (bool, error) {
	f.binds = append(f.binds, [2]int{keyID, nodeID})
	if f.bindErr != nil {
		return false, f.bindErr
	}
	return true, nil
}

// newNodeLinkHandler: BYON and gateway routing on, one tenant (owner-1) with one
// enrolled machine (node 7) that holds its secret, and a node key bound to it.
// Gateway is the real RedisGateway, so the tokens it derives are the real ones.
func newNodeLinkHandler(t *testing.T) (*WarpHandler, *nodeLinkFakeStore, []byte) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	owner := "owner-1"
	fs := &nodeLinkFakeStore{
		settings: map[string]string{"routing_mode": "gateway", "feature_byon_enabled": "true"},
		keys: map[string]*store.WarpAPIKey{
			"node-abc": {ID: 3, NodeID: "node-abc", OwnerID: owner, BoundNodeID: 7},
		},
		nodes:   map[int]*models.Node{7: {ID: 7, Token: "5f0c-node-uuid", OwnerID: &owner}},
		secrets: map[int]string{},
	}
	secret, err := redisacl.LoadOrCreateNodeSecret(fs, nodeLinkClusterSecret, 7)
	if err != nil {
		t.Fatalf("mint the node secret: %v", err)
	}
	state := &AppState{
		Store:          fs,
		Redis:          rdb,
		FeatureFlags:   services.NewFeatureFlags(fs),
		Gateway:        services.NewRedisGateway(rdb, fs, nodeLinkClusterSecret),
		ClusterSecret:  nodeLinkClusterSecret,
		ACLProvisioner: redisacl.NewProvisioner(rdb),
		SuspendGrace:   48 * time.Hour,
	}
	return NewWarpHandler(state, nil), fs, secret
}

func linkBootAs(h *WarpHandler, key store.WarpAPIKey) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/warp/link-boot", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.LinkBoot(rec, r.WithContext(context.WithValue(r.Context(), warpKeyCtx, key)))
	return rec
}

// A bound node key gets its node's OWN Link: the same derived token and node id
// the node-managed Link runs on, so routes do not move, and the node-link Redis
// user Core already provisions for that node. Every value is pinned against its
// derivation, so the route-only shape - or anything keyed by the overlay
// identity instead of the node - fails here.
func TestLinkBoot_BoundNodeKeyGetsItsNodesLink(t *testing.T) {
	h, fs, secret := newNodeLinkHandler(t)

	rec := linkBootAs(h, *fs.keys["node-abc"])

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	const token = "5f0c-node-uuid"
	want := map[string]string{
		"link_token":           services.DeriveLinkToken(token, nodeLinkClusterSecret),
		"node_id":              token,
		"link_discovery_proof": services.DeriveDiscoveryProof(token, nodeLinkClusterSecret),
		"redis_user":           redisacl.LinkUsername(token),
		"redis_pass":           redisacl.LinkPassword(secret, token),
		"redis_addr":           "host.docker.internal:25571",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["redis_db"]; !ok {
		t.Error("redis_db is missing")
	}
	// link_id is the route-only answer's overlay identity. A BYON Link is told
	// its node, never the key it booted with.
	if _, ok := got["link_id"]; ok {
		t.Errorf("the answer carries link_id %v", got["link_id"])
	}
}

// A kit starts the Link beside a node that may not have enrolled yet, so "not
// bound" is the normal first answer: retryable, and without touching a node.
func TestLinkBoot_UnboundNodeKeyIsAskedToRetry(t *testing.T) {
	h, fs, _ := newNodeLinkHandler(t)
	key := *fs.keys["node-abc"]
	key.BoundNodeID = 0

	rec := linkBootAs(h, key)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if fs.nodeLookups != 0 {
		t.Error("an unbound key looked a node up")
	}
}

// The same suspension gate route-only keys pass through, before any node is read.
func TestLinkBoot_NodeKeyOfASuspendedOwnerIsRefused(t *testing.T) {
	h, fs, _ := newNodeLinkHandler(t)
	at := time.Now().Add(-49 * time.Hour)
	fs.billing = &store.UserBilling{Status: "suspended", SuspendedAt: &at}

	rec := linkBootAs(h, *fs.keys["node-abc"])

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if fs.nodeLookups != 0 {
		t.Error("a suspended owner's key still looked its node up")
	}
}

// Ownership is asked again at every boot, not only when the key was bound: a
// machine that changed hands must not keep booting on its old owner's key.
func TestLinkBoot_NodeKeyOfAnotherOwnersMachineIsRefused(t *testing.T) {
	h, fs, _ := newNodeLinkHandler(t)
	other := "owner-2"
	fs.nodes[7].OwnerID = &other

	rec := linkBootAs(h, *fs.keys["node-abc"])

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "redis_pass") {
		t.Error("a refusal carried a credential")
	}
}

// A node row with no secret has not finished enrolling, or its pairing was
// reset; its Link credential cannot be derived yet, so the Link waits rather
// than giving up, and the message names both causes.
func TestLinkBoot_NodeWithoutASecretYetIsAskedToRetry(t *testing.T) {
	h, fs, _ := newNodeLinkHandler(t)
	delete(fs.secrets, 7)

	rec := linkBootAs(h, *fs.keys["node-abc"])
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	for _, cause := range []string{"still enrolling", "pairing was reset"} {
		if !strings.Contains(rec.Body.String(), cause) {
			t.Errorf("the 409 does not name %q: %s", cause, rec.Body.String())
		}
	}
}

// A route-only key keeps its own path: no node is ever read for it, and it goes
// on to provision the route-only ACL. miniredis has no ACL command, so that
// provisioning step is where this stops - which is the proof it got there.
func TestLinkBoot_RouteOnlyKeyKeepsItsOwnPath(t *testing.T) {
	h, fs, _ := newNodeLinkHandler(t)
	fs.keys["link-xyz"] = &store.WarpAPIKey{ID: 9, NodeID: "link-xyz", OwnerID: "owner-1"}

	rec := linkBootAs(h, *fs.keys["link-xyz"])

	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "Failed to provision credentials") {
		t.Fatalf("status = %d (%s), want the route-only provisioning step", rec.Code, rec.Body.String())
	}
	if fs.nodeLookups != 0 {
		t.Error("a route-only key looked a node up")
	}
}

// Every key that is neither a route-only kit nor a tenant's node key is refused
// as it always was. The owner-less node key is the one that matters: a platform
// key has no BYON node behind it, whatever its identity looks like.
func TestLinkBoot_OtherKeysAreRefused(t *testing.T) {
	for name, key := range map[string]store.WarpAPIKey{
		"owner-less node key": {ID: 11, NodeID: "node-platform"},
		"platform key":        {ID: 12},
		"some other identity": {ID: 13, NodeID: "edge-1", OwnerID: "owner-1"},
	} {
		t.Run(name, func(t *testing.T) {
			h, fs, _ := newNodeLinkHandler(t)
			rec := linkBootAs(h, key)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if fs.nodeLookups != 0 {
				t.Error("a refused key looked a node up")
			}
		})
	}
}

func bindReq(nodeKey, userID, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/warp/node-keys/"+nodeKey+"/bind", strings.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"nodeID": nodeKey})
	return r.WithContext(context.WithValue(r.Context(), "userID", userID))
}

// Binding joins a key and a machine, so both ends are owner-checked, and a key
// or machine that already has a partner is refused rather than moved.
func TestBindNodeWarpKey_OwnerChecks(t *testing.T) {
	owner, other := "owner-1", "owner-2"
	revokedAt := time.Now()
	tests := []struct {
		name       string
		key        store.WarpAPIKey
		extra      *store.WarpAPIKey
		node       int
		wantStatus int
		wantBind   bool
	}{
		{name: "own key to own machine", key: store.WarpAPIKey{ID: 20, NodeID: "node-free", OwnerID: owner}, node: 8, wantStatus: http.StatusOK, wantBind: true},
		{name: "someone else's key", key: store.WarpAPIKey{ID: 20, NodeID: "node-free", OwnerID: other}, node: 8, wantStatus: http.StatusNotFound},
		{name: "someone else's machine", key: store.WarpAPIKey{ID: 20, NodeID: "node-free", OwnerID: owner}, node: 9, wantStatus: http.StatusNotFound},
		{name: "an operator machine", key: store.WarpAPIKey{ID: 20, NodeID: "node-free", OwnerID: owner}, node: 10, wantStatus: http.StatusNotFound},
		{name: "a machine that does not exist", key: store.WarpAPIKey{ID: 20, NodeID: "node-free", OwnerID: owner}, node: 99, wantStatus: http.StatusNotFound},
		{name: "a key bound to another machine", key: store.WarpAPIKey{ID: 20, NodeID: "node-free", OwnerID: owner, BoundNodeID: 7}, node: 8, wantStatus: http.StatusConflict},
		{
			name: "a machine that already has a key", key: store.WarpAPIKey{ID: 20, NodeID: "node-free", OwnerID: owner},
			extra: &store.WarpAPIKey{ID: 22, NodeID: "node-other", OwnerID: owner, BoundNodeID: 8}, node: 8, wantStatus: http.StatusConflict,
		},
		{name: "a revoked key", key: store.WarpAPIKey{ID: 20, NodeID: "node-free", OwnerID: owner, RevokedAt: &revokedAt}, node: 8, wantStatus: http.StatusConflict},
		{name: "already bound to this machine", key: store.WarpAPIKey{ID: 20, NodeID: "node-free", OwnerID: owner, BoundNodeID: 8}, node: 8, wantStatus: http.StatusOK},
		{name: "a route-only key", key: store.WarpAPIKey{ID: 20, NodeID: "link-free", OwnerID: owner}, node: 8, wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, fs, _ := newNodeLinkHandler(t)
			fs.nodes[8] = &models.Node{ID: 8, Token: "t8", OwnerID: &owner}
			fs.nodes[9] = &models.Node{ID: 9, Token: "t9", OwnerID: &other}
			fs.nodes[10] = &models.Node{ID: 10, Token: "t10"}
			key := tt.key
			fs.keys[key.NodeID] = &key
			if tt.extra != nil {
				fs.keys[tt.extra.NodeID] = tt.extra
			}

			rec := httptest.NewRecorder()
			h.BindNodeWarpKey(rec, bindReq(key.NodeID, owner, fmt.Sprintf(`{"node":%d}`, tt.node)))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantBind {
				if len(fs.binds) != 1 || fs.binds[0] != [2]int{key.ID, tt.node} {
					t.Fatalf("binds = %v, want [[%d %d]]", fs.binds, key.ID, tt.node)
				}
			} else if len(fs.binds) != 0 {
				t.Fatalf("binds = %v, want none", fs.binds)
			}
		})
	}
}

// The pre-check lists only the caller's own keys, so a key someone else bound
// before the machine changed hands - or the enrol-time bind winning a race - is
// found by the unique index, not by the pre-check. That is a taken machine, which
// the owner can act on, not a server fault.
func TestBindNodeWarpKey_TakenMachineIsAConflictNotAFault(t *testing.T) {
	h, fs, _ := newNodeLinkHandler(t)
	owner := "owner-1"
	fs.nodes[8] = &models.Node{ID: 8, Token: "t8", OwnerID: &owner}
	fs.keys["node-free"] = &store.WarpAPIKey{ID: 20, NodeID: "node-free", OwnerID: owner}
	fs.bindErr = store.ErrWarpKeyNodeTaken

	rec := httptest.NewRecorder()
	h.BindNodeWarpKey(rec, bindReq("node-free", owner, `{"node":8}`))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "This machine already has a key for its Link") {
		t.Errorf("body = %s, want the taken-machine message", rec.Body.String())
	}
}

// BYON off means no tenant owns a machine, so there is nothing to bind.
func TestBindNodeWarpKey_RefusedWithBYONOff(t *testing.T) {
	h, fs, _ := newNodeLinkHandler(t)
	fs.settings["feature_byon_enabled"] = "false"

	rec := httptest.NewRecorder()
	h.BindNodeWarpKey(rec, bindReq("node-abc", "owner-1", `{"node":7}`))

	if rec.Code != http.StatusForbidden || len(fs.binds) != 0 {
		t.Fatalf("status = %d, binds = %v; want 403 and none", rec.Code, fs.binds)
	}
}
