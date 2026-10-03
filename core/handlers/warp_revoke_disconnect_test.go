package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	keysByNodeID    map[string]*store.WarpAPIKey
	peersByKey      map[int][]store.WarpPeer
	revoked         []string
	deletedRegions  []string
	deletedLeaders  []string
	leadersByRegion map[string][]store.WarpLeader
	regions         map[string]bool
	leaders         map[string]bool
}

func (f *revokeFakeStore) ListWarpLeadersByRegion(region string) ([]store.WarpLeader, error) {
	return f.leadersByRegion[region], nil
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

// The real store reports whether a row went away, so the fake keeps a set of
// the regions and leaders that exist rather than accepting every delete.
func (f *revokeFakeStore) DeleteWarpRegion(region string) (bool, error) {
	f.deletedRegions = append(f.deletedRegions, region)
	if !f.regions[region] {
		return false, nil
	}
	delete(f.regions, region)
	return true, nil
}

func (f *revokeFakeStore) DeleteWarpLeader(leaderID string) (bool, error) {
	f.deletedLeaders = append(f.deletedLeaders, leaderID)
	if !f.leaders[leaderID] {
		return false, nil
	}
	delete(f.leaders, leaderID)
	return true, nil
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
	fs.leadersByRegion = map[string][]store.WarpLeader{}
	// The region and leader the delete tests act on exist unless a test says
	// otherwise, so a 404 means "this fixture never had it" rather than "the
	// fake accepts anything".
	fs.regions = map[string]bool{"leader-01": true}
	fs.leaders = map[string]bool{"leader-01": true}
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

// A warp leader REGISTERS ITSELF: it writes its region and endpoint into the
// liveness key it refreshes, and the self-registrar creates a row for any leader
// it has never seen, enabled. So deleting the row of a leader whose process is
// still running is not a deletion - it comes back within selfRegInterval, and the
// panel's confirm dialog promised the endpoint "has to be re-entered by hand".
//
// Measured on production before this refusal existed: eu-edge-02 deleted at T+0,
// absent at T+15s, back at T+30s with enabled=true.
//
// Disable is the durable decision (a disabled row is never re-enabled by its own
// heartbeat), so the refusal points at it.
func TestDeletingALeaderThatStillAnnouncesIsRefused(t *testing.T) {
	h, fs := newRevokeTestHandler(t)
	if err := h.state.Redis.Set(context.Background(), "dylaris:warp:leader-01:alive", "1", 0).Err(); err != nil {
		t.Fatalf("seed liveness: %v", err)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/warp/leaders/leader-01", nil)
	r = mux.SetURLVars(r, map[string]string{"leaderId": "leader-01"})
	h.DeleteLeader(rec, r)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if len(fs.deletedLeaders) != 0 {
		t.Errorf("the row was deleted anyway: %v", fs.deletedLeaders)
	}
	// The operator has to be told what to do instead, or the refusal is just a
	// different way of getting nowhere.
	if !strings.Contains(rec.Body.String(), "disable") {
		t.Errorf("the refusal does not name the durable action: %s", rec.Body.String())
	}
}

// The same leader once it has stopped announcing: nothing re-creates it, so the
// delete is real and is allowed.
func TestDeletingALeaderThatStoppedAnnouncingWorks(t *testing.T) {
	h, fs := newRevokeTestHandler(t)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/warp/leaders/leader-01", nil)
	r = mux.SetURLVars(r, map[string]string{"leaderId": "leader-01"})
	h.DeleteLeader(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.deletedLeaders) != 1 {
		t.Errorf("nothing was deleted: %v", fs.deletedLeaders)
	}
}

// Deleting something that is not there reported success, for both the leader and
// the region endpoint. An operator cannot tell a typo from a delete, and a second
// tab that already removed the row looks like it removed it twice.
func TestDeletingWarpTopologyThatDoesNotExistIs404(t *testing.T) {
	t.Run("leader", func(t *testing.T) {
		h, _ := newRevokeTestHandler(t)
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodDelete, "/api/warp/leaders/no-such-leader", nil)
		r = mux.SetURLVars(r, map[string]string{"leaderId": "no-such-leader"})
		h.DeleteLeader(rec, r)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("region", func(t *testing.T) {
		h, _ := newRevokeTestHandler(t)
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodDelete, "/api/warp/regions/no-such-region", nil)
		r = mux.SetURLVars(r, map[string]string{"region": "no-such-region"})
		h.DeleteRegion(rec, r)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})
}

// A region is re-created the same way, out of the announcement of any leader that
// is still in it - and deleting it cascades those leader rows away first, so the
// self-registrar rebuilds both. The peer guard beside this one does not catch it:
// a region with live leaders and no enrolled peers passes that check.
func TestDeletingARegionWhoseLeaderStillAnnouncesIsRefused(t *testing.T) {
	h, fs := newRevokeTestHandler(t)
	fs.leadersByRegion["leader-01"] = []store.WarpLeader{{LeaderID: "leader-01", Region: "leader-01"}}
	if err := h.state.Redis.Set(context.Background(), "dylaris:warp:leader-01:alive", "1", 0).Err(); err != nil {
		t.Fatalf("seed liveness: %v", err)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/warp/regions/leader-01", nil)
	r = mux.SetURLVars(r, map[string]string{"region": "leader-01"})
	h.DeleteRegion(rec, r)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if len(fs.deletedRegions) != 0 {
		t.Errorf("the region was deleted anyway: %v", fs.deletedRegions)
	}
}

// linkRevokeFakeGateway satisfies services.GatewayProvider for the teardown.
type linkRevokeFakeGateway struct{}

func (linkRevokeFakeGateway) CreateServerRoute(uint, string, string, int) error { return nil }
func (linkRevokeFakeGateway) CreateRouteViaLink(string, string, string, string, int) error {
	return nil
}
func (linkRevokeFakeGateway) DeleteCoreOwnedRoute(string) error     { return nil }
func (linkRevokeFakeGateway) DeleteRoute(string) error              { return nil }
func (linkRevokeFakeGateway) DeleteServerRoutes(string) error       { return nil }
func (linkRevokeFakeGateway) MigrateServerRoutes(uint, uint) error  { return nil }
func (linkRevokeFakeGateway) LinkToken(nodeID string) string        { return "tok-" + nodeID }
func (linkRevokeFakeGateway) DiscoveryProof(nodeID string) string   { return "proof-" + nodeID }
func (linkRevokeFakeGateway) SetNodeLinkEnabled(string, bool) error { return nil }

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
