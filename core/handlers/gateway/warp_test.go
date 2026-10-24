package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/handlers"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/services"
	"dylaris-core/store"
)

// warpFakeStore embeds store.Store (nil) so it satisfies the full interface at
// compile time; only the methods the warp handler/service touch are overridden.
// Any other method call would panic — the test never makes one.
type warpFakeStore struct {
	store.Store
	settings map[string]string
	peers    map[string]store.WarpPeer
	nextID   int

	billing        *store.UserBilling
	billingErr     error
	billingLookups []string
}

func (f *warpFakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	f.billingLookups = append(f.billingLookups, userID)
	return f.billing, f.billingErr
}

func (f *warpFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *warpFakeStore) InsertWarpPeer(p store.WarpPeer) (int, error) {
	f.nextID++
	p.ID = f.nextID
	f.peers[p.Pubkey] = p
	return p.ID, nil
}
func (f *warpFakeStore) GetWarpPeerByPubkey(pk string) (*store.WarpPeer, error) {
	if p, ok := f.peers[pk]; ok {
		return &p, nil
	}
	return nil, warpErr("not found")
}
func (f *warpFakeStore) DeleteWarpPeerByPubkey(pk string) error { delete(f.peers, pk); return nil }
func (f *warpFakeStore) SetWarpPeerAssignedLeader(pubkey, leaderID string) error {
	p, ok := f.peers[pubkey]
	if !ok {
		return warpErr("not found")
	}
	p.AssignedLeader = leaderID
	f.peers[pubkey] = p
	return nil
}

// Multi-hub: a single seeded region "leader-01" (10.0.99.0/24) with one leader.
func (f *warpFakeStore) ListWarpRegions() ([]store.WarpRegion, error) {
	return []store.WarpRegion{{Region: "leader-01", Subnet: "10.0.99.0/24", Enabled: true}}, nil
}
func (f *warpFakeStore) GetWarpRegion(region string) (*store.WarpRegion, error) {
	if region == "leader-01" {
		return &store.WarpRegion{Region: "leader-01", Subnet: "10.0.99.0/24", Enabled: true}, nil
	}
	return nil, warpErr("no such region")
}
func (f *warpFakeStore) ListWarpLeaders() ([]store.WarpLeader, error) {
	return []store.WarpLeader{{LeaderID: "leader-01", Region: "leader-01", Endpoint: "vpn.example.com:51820", Enabled: true}}, nil
}
func (f *warpFakeStore) CountWarpPeersByRegion() (map[string]int, error) {
	out := map[string]int{}
	for _, p := range f.peers {
		out[p.Region]++
	}
	return out, nil
}
func (f *warpFakeStore) EnrollPeerTx(keyID, limit int, onNewConn, pubkey, fixedIP, region string, allocIP func(taken map[string]bool) (string, error)) (string, string, error) {
	wgIP := fixedIP
	if wgIP == "" {
		taken := map[string]bool{}
		for _, p := range f.peers {
			taken[p.WGIP] = true
		}
		ip, err := allocIP(taken)
		if err != nil {
			return "", "", err
		}
		wgIP = ip
	}
	_, _ = f.InsertWarpPeer(store.WarpPeer{APIKeyID: keyID, Pubkey: pubkey, WGIP: wgIP, Region: region})
	return wgIP, "", nil
}

func (f *warpFakeStore) CreateWarpAPIKey(k store.WarpAPIKey) (int, error) {
	f.nextID++
	return f.nextID, nil
}

type warpErr string

func (e warpErr) Error() string { return string(e) }

func adminMintReq(body map[string]interface{}) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/api/admin/warp/keys", bytes.NewReader(b))
	return r.WithContext(context.WithValue(r.Context(), "isAdmin", true))
}

func TestMintAPIKey_FixedWGIP_RequiresRegion(t *testing.T) {
	h := newWarpTestHandler(t)
	rec := httptest.NewRecorder()
	h.MintAPIKey(rec, adminMintReq(map[string]interface{}{"fixed_wg_ip": "10.0.99.50"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestMintAPIKey_FixedWGIP_Invalid(t *testing.T) {
	h := newWarpTestHandler(t)
	rec := httptest.NewRecorder()
	// .1 is the leader-reserved first host of 10.0.99.0/24.
	h.MintAPIKey(rec, adminMintReq(map[string]interface{}{"fixed_wg_ip": "10.0.99.1", "region": "leader-01"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestMintAPIKey_FixedWGIP_Valid(t *testing.T) {
	h := newWarpTestHandler(t)
	rec := httptest.NewRecorder()
	h.MintAPIKey(rec, adminMintReq(map[string]interface{}{"fixed_wg_ip": "10.0.99.50", "region": "leader-01"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func newWarpTestHandler(t *testing.T) *WarpHandler {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	fs := &warpFakeStore{
		// Enrollment requires gateway routing; set it so the happy-path test
		// exercises a fully enabled platform.
		settings: map[string]string{"routing_mode": "gateway"},
		peers:    map[string]store.WarpPeer{},
	}
	svc := services.NewWarpService(fs, rdb, "test-secret")
	state := &handlers.AppState{Store: fs, Redis: rdb}
	return NewWarpHandler(state, svc)
}

func withTestWarpKey(r *http.Request) *http.Request {
	key := store.WarpAPIKey{ID: 1, Policy: "general", MaxConns: 5, OnNewConn: "block"}
	return r.WithContext(context.WithValue(r.Context(), warpKeyCtx, key))
}

func TestEnroll_HappyPath_ReturnsConfigWithLeaderInfo(t *testing.T) {
	h := newWarpTestHandler(t)
	body, _ := json.Marshal(map[string]interface{}{
		"public_key":     "clientPubKey",
		"tunnel_subnets": []string{"10.0.0.0/24"},
	})
	req := httptest.NewRequest("POST", "/api/warp/enroll", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Enroll(rec, withTestWarpKey(req))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	endpoints, _ := resp["endpoints"].([]interface{})
	if resp["wg_ip"] == "" || resp["leader_public_key"] == "" || len(endpoints) == 0 || endpoints[0] != "vpn.example.com:51820" {
		t.Fatalf("missing leader info in response: %+v", resp)
	}
}

// enrollWithOwner drives Enroll with a tenant-owned key, the only kind a
// suspension can apply to.
func enrollWithOwner(t *testing.T, h *WarpHandler) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"public_key": "ownedPubKey"})
	req := httptest.NewRequest("POST", "/api/warp/enroll", bytes.NewReader(body))
	key := store.WarpAPIKey{ID: 2, Policy: "general", MaxConns: 5, OnNewConn: "block", OwnerID: "u1"}
	rec := httptest.NewRecorder()
	h.Enroll(rec, req.WithContext(context.WithValue(req.Context(), warpKeyCtx, key)))
	return rec
}

// The tunnel is what the tenant keeps for the grace period and loses after it.
// The enforcement pass drops their peers; without this gate the client would
// re-enroll within minutes and put them straight back.
func TestEnroll_HardSuspendedOwnerIsRefused(t *testing.T) {
	h := newWarpTestHandler(t)
	fs := h.state.Store.(*warpFakeStore)
	h.state.SuspendGrace = 48 * time.Hour
	at := time.Now().Add(-49 * time.Hour)
	fs.billing = &store.UserBilling{Status: "suspended", SuspendedAt: &at}

	if rec := enrollWithOwner(t, h); rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// Within the grace the tunnel stays up - that is the entire point of a grace,
// and it matches how LinkBoot and the ACL reconciler treat the same window.
func TestEnroll_SuspendedWithinGraceStillEnrolls(t *testing.T) {
	h := newWarpTestHandler(t)
	fs := h.state.Store.(*warpFakeStore)
	h.state.SuspendGrace = 48 * time.Hour
	at := time.Now().Add(-1 * time.Hour)
	fs.billing = &store.UserBilling{Status: "suspended", SuspendedAt: &at}

	if rec := enrollWithOwner(t, h); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// A DB fault must not lock out paying tenants on reconnect. It fails OPEN, and
// the log line is what makes the degraded gate visible.
func TestEnroll_BillingLookupFailureFailsOpen(t *testing.T) {
	h := newWarpTestHandler(t)
	fs := h.state.Store.(*warpFakeStore)
	fs.billingErr = errors.New("db down")

	if rec := enrollWithOwner(t, h); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// A platform key has no billing row at all; suspension is a tenant concept, so
// the gate must not even look.
func TestEnroll_PlatformKeySkipsTheSuspensionGate(t *testing.T) {
	h := newWarpTestHandler(t)
	fs := h.state.Store.(*warpFakeStore)

	body, _ := json.Marshal(map[string]interface{}{"public_key": "platformPubKey"})
	req := httptest.NewRequest("POST", "/api/warp/enroll", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Enroll(rec, withTestWarpKey(req))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.billingLookups) != 0 {
		t.Errorf("looked up billing for an unowned key: %v", fs.billingLookups)
	}
}
