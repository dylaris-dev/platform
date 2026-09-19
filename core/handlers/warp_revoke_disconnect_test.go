package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// Revoking a warp key only blocks the NEXT enroll. WireGuard carries no memory of
// the key that created a tunnel, so an established peer keeps forwarding until it
// is pushed out of the leaders - which is what DisconnectKeyPeers exists for and
// what its own doc calls the difference between a security control and the
// appearance of one.
//
// Two of the four revoke paths called it (RevokeAPIKey, DeleteAPIKey) and the
// suspension cutoff calls it. The two TENANT-facing ones did not, so the door a
// customer actually uses after losing a key left the machine on the overlay
// indefinitely - while RevokeNodeWarpKey's own comment claimed the tunnel dropped
// "at reconnect", which nothing implements: a re-enroll under a revoked key is
// refused at the middleware and the refusal touches neither the peer row nor the
// leader.

// revokeFakeStore extends the warp fake with the key lookups the revoke handlers
// need, plus a record of which keys were revoked.
type revokeFakeStore struct {
	warpFakeStore
	keysByNodeID   map[string]*store.WarpAPIKey
	peersByKey     map[int][]store.WarpPeer
	revoked        []string
	deletedRegions []string
}

// The revoke teardown enumerates the stored route-only rows rather than the
// live routing table, so a cache that lost them still gets a complete
// revocation. This fixture has none.
func (f *revokeFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return nil, nil
}

func (f *revokeFakeStore) GetWarpAPIKeyByNodeID(nodeID string) (*store.WarpAPIKey, error) {
	k, ok := f.keysByNodeID[nodeID]
	if !ok {
		return nil, warpErr("not found")
	}
	return k, nil
}

func (f *revokeFakeStore) RevokeWarpAPIKeyByNodeID(nodeID string) error {
	f.revoked = append(f.revoked, nodeID)
	return nil
}

func (f *revokeFakeStore) ListWarpPeersByKey(keyID int) ([]store.WarpPeer, error) {
	return f.peersByKey[keyID], nil
}

func (f *revokeFakeStore) ListWarpPeersByRegion(region string) ([]store.WarpPeer, error) {
	var out []store.WarpPeer
	for _, ps := range f.peersByKey {
		for _, p := range ps {
			if p.Region == region {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

func (f *revokeFakeStore) DeleteWarpRegion(region string) error {
	f.deletedRegions = append(f.deletedRegions, region)
	return nil
}

// seedPeer registers a peer in BOTH maps: peersByKey is what ListWarpPeersByKey
// enumerates, f.peers is what DeleteWarpPeerByPubkey actually removes from. A
// test that seeds only the first asserts nothing when it later checks f.peers -
// the key was never there to begin with.
func (f *revokeFakeStore) seedPeer(keyID int, p store.WarpPeer) {
	p.APIKeyID = keyID
	f.peersByKey[keyID] = append(f.peersByKey[keyID], p)
	f.peers[p.Pubkey] = p
}

func newRevokeTestHandler(t *testing.T) (*WarpHandler, *revokeFakeStore) {
	t.Helper()
	base := newWarpTestHandler(t)
	fs := &revokeFakeStore{
		warpFakeStore: *base.state.Store.(*warpFakeStore),
		keysByNodeID:  map[string]*store.WarpAPIKey{},
		peersByKey:    map[int][]store.WarpPeer{},
	}
	// The service is rebuilt against the EXTENDED fake, so DisconnectKeyPeers
	// resolves peers through it rather than through the base fake.
	svc := services.NewWarpService(fs, base.state.Redis, "test-secret")
	state := &AppState{Store: fs, Redis: base.state.Redis, FeatureFlags: services.NewFeatureFlags(fs)}
	return NewWarpHandler(state, svc), fs
}

func revokeReq(t *testing.T, path, varName, varValue, userID string, isAdmin bool) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, path, nil)
	r = mux.SetURLVars(r, map[string]string{varName: varValue})
	ctx := context.WithValue(r.Context(), "userID", userID)
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	return r.WithContext(ctx)
}

func TestRevokeNodeWarpKeyDisconnectsThePeer(t *testing.T) {
	h, fs := newRevokeTestHandler(t)
	fs.settings["feature_byon_enabled"] = "true"
	fs.keysByNodeID["node-abc"] = &store.WarpAPIKey{ID: 7, NodeID: "node-abc", OwnerID: "owner-1"}
	fs.seedPeer(7, store.WarpPeer{Pubkey: "pk1", WGIP: "10.0.99.5", Region: "leader-01"})

	rec := httptest.NewRecorder()
	h.RevokeNodeWarpKey(rec, revokeReq(t, "/api/warp/node-keys/node-abc", "nodeID", "node-abc", "owner-1", false))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Disconnected int `json:"disconnected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Disconnected != 1 {
		t.Fatalf("disconnected = %d, want 1 - the machine keeps its overlay tunnel", body.Disconnected)
	}
	// The peer row is what a leader resync rebuilds from, so it has to be gone too.
	if _, still := fs.peers["pk1"]; still {
		t.Error("the peer row survived the revoke; a resync would put the tunnel back")
	}
	if len(fs.revoked) != 1 {
		t.Errorf("the durable revoke did not run: %v", fs.revoked)
	}
}

// The durable revoke must still be what decides success, so a revoke on a key
// that never enrolled anything is a clean 200 rather than an error.
func TestRevokeNodeWarpKeyWithNoPeersStillSucceeds(t *testing.T) {
	h, fs := newRevokeTestHandler(t)
	fs.settings["feature_byon_enabled"] = "true"
	fs.keysByNodeID["node-unused"] = &store.WarpAPIKey{ID: 9, NodeID: "node-unused", OwnerID: "owner-1"}

	rec := httptest.NewRecorder()
	h.RevokeNodeWarpKey(rec, revokeReq(t, "/api/warp/node-keys/node-unused", "nodeID", "node-unused", "owner-1", false))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.revoked) != 1 {
		t.Errorf("the durable revoke did not run: %v", fs.revoked)
	}
}

// Deleting a warp region cascades its LEADER rows away while the peer rows
// survive (warp_peers.region has no foreign key). With no leader row left,
// pushToRegion matches nothing and silently sends nothing, so every later
// disconnect reports success and removes no tunnel. The refusal is the only exit.
func TestDeleteWarpRegionRefusesWhilePeersAreEnrolled(t *testing.T) {
	h, fs := newRevokeTestHandler(t)
	fs.seedPeer(7, store.WarpPeer{Pubkey: "pk1", WGIP: "10.0.99.5", Region: "leader-01"})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/admin/warp/regions/leader-01", nil)
	r = mux.SetURLVars(r, map[string]string{"region": "leader-01"})
	h.DeleteRegion(rec, r)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteWarpRegionAllowedWhenEmpty(t *testing.T) {
	h, _ := newRevokeTestHandler(t)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/admin/warp/regions/leader-01", nil)
	r = mux.SetURLVars(r, map[string]string{"region": "leader-01"})
	h.DeleteRegion(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// linkRevokeFakeGateway satisfies services.GatewayProvider for the teardown.
type linkRevokeFakeGateway struct{}

func (linkRevokeFakeGateway) CreateServerRoute(uint, string, string, int) error { return nil }
func (linkRevokeFakeGateway) CreateRouteViaLink(string, string, string, string, int) error {
	return nil
}
func (linkRevokeFakeGateway) DeleteCoreOwnedRoute(string) error    { return nil }
func (linkRevokeFakeGateway) DeleteRoute(string) error             { return nil }
func (linkRevokeFakeGateway) DeleteServerRoutes(string) error      { return nil }
func (linkRevokeFakeGateway) MigrateServerRoutes(uint, uint) error { return nil }
func (linkRevokeFakeGateway) LinkToken(nodeID string) string       { return "tok-" + nodeID }
func (linkRevokeFakeGateway) DiscoveryProof(nodeID string) string  { return "proof-" + nodeID }

// A kit key cannot enroll any more, but a kit from the warp era may still be an
// overlay member, and revoking it must take that away as well.
func TestRevokeLinkKitDisconnectsThePeer(t *testing.T) {
	h, fs := newRevokeTestHandler(t)
	fs.settings["feature_byon_enabled"] = "true"
	fs.keysByNodeID["link-abc"] = &store.WarpAPIKey{ID: 11, NodeID: "link-abc", OwnerID: "owner-1"}
	fs.seedPeer(11, store.WarpPeer{Pubkey: "pk-link", WGIP: "10.0.99.9", Region: "leader-01"})
	h.state.Gateway = linkRevokeFakeGateway{}

	rec := httptest.NewRecorder()
	h.RevokeLinkKit(rec, revokeReq(t, "/api/warp/link-kits/link-abc", "linkID", "link-abc", "owner-1", false))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, still := fs.peers["pk-link"]; still {
		t.Error("the peer row survived; the machine stays an overlay member after the kit was revoked")
	}
	if len(fs.revoked) != 1 {
		t.Errorf("the durable revoke did not run: %v", fs.revoked)
	}
}
