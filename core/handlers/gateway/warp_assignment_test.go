package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/services"
	"dylaris-core/store"
)

func contains(s, substr string) bool { return strings.Contains(s, substr) }

// assignmentFakeStore extends warpFakeStore with a single configurable region +
// leader and a key-hash lookup. warpFakeStore hardcodes region "leader-01" for
// the enroll tests; Assignment needs to resolve the peer's OWN stored region, so
// the region/leader here are parameterized per test instead.
type assignmentFakeStore struct {
	*warpFakeStore
	region store.WarpRegion
	leader store.WarpLeader
	key    store.WarpAPIKey
	hash   string
}

func (f *assignmentFakeStore) ListWarpRegions() ([]store.WarpRegion, error) {
	return []store.WarpRegion{f.region}, nil
}

func (f *assignmentFakeStore) GetWarpRegion(region string) (*store.WarpRegion, error) {
	if region == f.region.Region {
		r := f.region
		return &r, nil
	}
	return nil, warpErr("no such region")
}

func (f *assignmentFakeStore) ListWarpLeaders() ([]store.WarpLeader, error) {
	return []store.WarpLeader{f.leader}, nil
}

func (f *assignmentFakeStore) GetWarpAPIKeyByHash(hash string) (*store.WarpAPIKey, error) {
	if hash == f.hash {
		k := f.key
		return &k, nil
	}
	return nil, warpErr("not found")
}

// newTestWarpHandlerWithPeer builds a WarpHandler backed by a fake store seeded
// with one warp peer (owned by keyID, pinned to region/leaderID) and one warp API
// key whose plaintext bearer token is "PLAINTEXT-FOR-KEY-<keyID>", so tests can
// drive the real WarpAPIKeyMiddleware -> Assignment handler chain.
func newTestWarpHandlerWithPeer(t *testing.T, keyID int, pubkey, region, leaderID string) *WarpHandler {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	base := &warpFakeStore{
		settings: map[string]string{"routing_mode": "gateway"},
		peers: map[string]store.WarpPeer{
			pubkey: {ID: 1, APIKeyID: keyID, Pubkey: pubkey, WGIP: "10.0.99.50", Region: region, AssignedLeader: leaderID},
		},
	}
	fs := &assignmentFakeStore{
		warpFakeStore: base,
		region:        store.WarpRegion{Region: region, Subnet: "10.0.99.0/24", Enabled: true},
		leader:        store.WarpLeader{LeaderID: leaderID, Region: region, Endpoint: "vpn.example.com:51820", Enabled: true},
		key:           store.WarpAPIKey{ID: keyID, Policy: "general", MaxConns: 5, OnNewConn: "block"},
		hash:          handlers.HashAPIKey(fmt.Sprintf("PLAINTEXT-FOR-KEY-%d", keyID)),
	}
	svc := services.NewWarpService(fs, rdb, "test-secret")
	state := &handlers.AppState{Store: fs, Redis: rdb}
	return NewWarpHandler(state, svc)
}

// newTestWarpHandlerWithForeignPeer builds a WarpHandler like
// newTestWarpHandlerWithPeer, except the seeded peer EXISTS but is owned by a
// different warp API key (foreignKeyID) than the one that authenticates the
// request (authKeyID). It exercises the ownership guard in
// (*WarpService).Assignment ("if peer.APIKeyID != key.ID"), which the plain
// unknown-pubkey case never reaches because GetWarpPeerByPubkey already fails
// before that check runs.
func newTestWarpHandlerWithForeignPeer(t *testing.T, authKeyID, foreignKeyID int, pubkey, region, leaderID string) *WarpHandler {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	base := &warpFakeStore{
		settings: map[string]string{"routing_mode": "gateway"},
		peers: map[string]store.WarpPeer{
			pubkey: {ID: 1, APIKeyID: foreignKeyID, Pubkey: pubkey, WGIP: "10.0.99.51", Region: region, AssignedLeader: leaderID},
		},
	}
	fs := &assignmentFakeStore{
		warpFakeStore: base,
		region:        store.WarpRegion{Region: region, Subnet: "10.0.99.0/24", Enabled: true},
		leader:        store.WarpLeader{LeaderID: leaderID, Region: region, Endpoint: "vpn.example.com:51820", Enabled: true},
		key:           store.WarpAPIKey{ID: authKeyID, Policy: "general", MaxConns: 5, OnNewConn: "block"},
		hash:          handlers.HashAPIKey(fmt.Sprintf("PLAINTEXT-FOR-KEY-%d", authKeyID)),
	}
	svc := services.NewWarpService(fs, rdb, "test-secret")
	state := &handlers.AppState{Store: fs, Redis: rdb}
	return NewWarpHandler(state, svc)
}

// Assignment returns 200 + the peer's endpoint order for an authenticated key
// whose peer exists; 404 when the pubkey is unknown or belongs to another key.
// The seeded peer has a non-empty AssignedLeader ("leader-b"), so the response
// must also come back assigned:true (F3 poll-drift fix).
func TestAssignment_ReturnsHomeFirstForOwnedPeer(t *testing.T) {
	h := newTestWarpHandlerWithPeer(t /* keyID */, 3, "PUBKEY", "eu-1", "leader-b") // helper: seeds a peer owned by key 3
	req := httptest.NewRequest(http.MethodGet, "/api/warp/assignment?public_key=PUBKEY", nil)
	req.Header.Set("Authorization", "Bearer PLAINTEXT-FOR-KEY-3")
	rr := httptest.NewRecorder()

	h.WarpAPIKeyMiddleware(h.Assignment)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"leader_public_key"`) {
		t.Fatalf("missing leader_public_key: %s", rr.Body.String())
	}
	var got services.EnrollResult
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rr.Body.String())
	}
	if !got.Assigned {
		t.Fatalf("Assigned = false, want true for a peer with a non-empty assigned_leader (body %s)", rr.Body.String())
	}
}

// TestAssignment_UnassignedForUnpinnedPeer is the F3 poll-drift fix: a peer with
// an EMPTY AssignedLeader (unpinned - pre-F3 migration default, a failed
// best-effort pin, etc.) must get assigned:false back, even though it still gets
// a full endpoint list (freest-first). The gateway client's checkAssignment
// no-ops on assigned:false so an unpinned peer no longer swaps/drifts on every
// 30s poll tick in a multi-leader region.
func TestAssignment_UnassignedForUnpinnedPeer(t *testing.T) {
	h := newTestWarpHandlerWithPeer(t, 3, "PUBKEY", "eu-1", "") // empty leaderID -> unpinned peer
	req := httptest.NewRequest(http.MethodGet, "/api/warp/assignment?public_key=PUBKEY", nil)
	req.Header.Set("Authorization", "Bearer PLAINTEXT-FOR-KEY-3")
	rr := httptest.NewRecorder()

	h.WarpAPIKeyMiddleware(h.Assignment)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var got services.EnrollResult
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rr.Body.String())
	}
	if got.Assigned {
		t.Fatalf("Assigned = true, want false for a peer with an empty assigned_leader (body %s)", rr.Body.String())
	}
	if len(got.Endpoints) == 0 {
		t.Fatalf("Endpoints empty, want the region's endpoint list even when unassigned (body %s)", rr.Body.String())
	}
}

// TestAssignment_404ForUnknownPubkey covers the "no such peer at all" branch:
// GetWarpPeerByPubkey itself errors because the pubkey was never enrolled.
// Distinct from TestAssignment_404ForForeignPeer below, where the peer DOES
// exist but is owned by a different key.
func TestAssignment_404ForUnknownPubkey(t *testing.T) {
	h := newTestWarpHandlerWithPeer(t, 3, "PUBKEY", "eu-1", "leader-b")
	req := httptest.NewRequest(http.MethodGet, "/api/warp/assignment?public_key=OTHER", nil)
	req.Header.Set("Authorization", "Bearer PLAINTEXT-FOR-KEY-3")
	rr := httptest.NewRecorder()

	h.WarpAPIKeyMiddleware(h.Assignment)(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// TestAssignment_404ForForeignPeer covers the ownership guard in
// (*WarpService).Assignment ("if peer.APIKeyID != key.ID") directly: the peer
// EXISTS (GetWarpPeerByPubkey succeeds) but is enrolled under a different warp
// API key (7) than the one authenticating the request (3).
func TestAssignment_404ForForeignPeer(t *testing.T) {
	h := newTestWarpHandlerWithForeignPeer(t /* authKeyID */, 3 /* foreignKeyID */, 7, "PUBKEY", "eu-1", "leader-b")
	req := httptest.NewRequest(http.MethodGet, "/api/warp/assignment?public_key=PUBKEY", nil)
	req.Header.Set("Authorization", "Bearer PLAINTEXT-FOR-KEY-3")
	rr := httptest.NewRecorder()

	h.WarpAPIKeyMiddleware(h.Assignment)(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
	}
}
