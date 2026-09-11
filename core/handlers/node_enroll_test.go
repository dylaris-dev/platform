package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/services"
	"dylaris-core/store"
)

// nodeEnrollFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only GetSetting (read by services.FeatureFlags
// for the byonActive gate) and CreateNodeEnrollToken (what MintToken writes)
// are overridden. Any other call would panic - these tests never make one.
type nodeEnrollFakeStore struct {
	store.Store

	settings map[string]string

	billing       *store.UserBilling
	nodes         int
	pendingTokens int
	warpNodeKeys  int

	createCalls []nodeEnrollCreateCall
	createErr   error

	// BYON node keys by identity, for the warpKeyNodeId validation.
	warpKeys map[string]*store.WarpAPIKey
}

type nodeEnrollCreateCall struct {
	userID        string
	plaintext     string
	label         string
	expiresAt     *time.Time
	warpKeyNodeID string
}

func (f *nodeEnrollFakeStore) GetWarpAPIKeyByNodeID(nodeID string) (*store.WarpAPIKey, error) {
	if k, ok := f.warpKeys[nodeID]; ok {
		return k, nil
	}
	return nil, errors.New("not found")
}

// The OTHER pending kind. A tenant can reach a node through a warp key just as
// well as through an enroll token, so the gate counts both and this fake has to
// be able to answer both - see services.NodeSlotsUsed.
func (f *nodeEnrollFakeStore) CountNodeWarpKeysByOwner(string) (int, error) {
	return f.warpNodeKeys, nil
}

func (f *nodeEnrollFakeStore) GetSetting(key string) (string, error) {
	return f.settings[key], nil
}

// The cap gate (MintToken counts existing nodes + redeemable tokens against
// max_nodes) runs before the token is generated. billing is where both the
// entitlement and the cap come from now; nil means an account that bought
// nothing, which these tests reach through the self-host path (StoreEnabled
// false) so they stay about the cap rather than about entitlement.
func (f *nodeEnrollFakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	return f.billing, nil
}
func (f *nodeEnrollFakeStore) CountNodesByOwner(ownerID string) (int, error) { return f.nodes, nil }
func (f *nodeEnrollFakeStore) CountPendingNodeEnrollTokens(userID string) (int, error) {
	return f.pendingTokens, nil
}

func (f *nodeEnrollFakeStore) CreateNodeEnrollToken(userID, plaintext, label string, expiresAt *time.Time, warpKeyNodeID string) error {
	f.createCalls = append(f.createCalls, nodeEnrollCreateCall{userID, plaintext, label, expiresAt, warpKeyNodeID})
	return f.createErr
}

// newNodeEnrollState builds an AppState with a real *services.FeatureFlags
// backed by the fake store (feature_byon_enabled drives byonActive), plus
// the GRPC TLS fields MintToken's fingerprint-disclosure gate reads.
func newNodeEnrollState(fs *nodeEnrollFakeStore, byonEnabled, grpcTLSEnabled bool, grpcFingerprint string) *AppState {
	if fs.settings == nil {
		fs.settings = map[string]string{}
	}
	fs.settings["feature_byon_enabled"] = "false"
	if byonEnabled {
		fs.settings["feature_byon_enabled"] = "true"
	}
	return &AppState{
		Store:              fs,
		FeatureFlags:       services.NewFeatureFlags(fs),
		GRPCTLSEnabled:     grpcTLSEnabled,
		GRPCTLSFingerprint: grpcFingerprint,
	}
}

func nodeEnrollMintReq(userID string, body map[string]interface{}) *http.Request {
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest("POST", "/api/nodes/enroll-token", bytes.NewReader(b))
	} else {
		r = httptest.NewRequest("POST", "/api/nodes/enroll-token", nil)
	}
	if userID != "" {
		r = r.WithContext(context.WithValue(r.Context(), "userID", userID))
	}
	return r
}

func TestMintToken_BYONInactiveForbidden(t *testing.T) {
	fs := &nodeEnrollFakeStore{}
	state := newNodeEnrollState(fs, false, false, "")
	h := NewNodeEnrollHandler(state)
	rec := httptest.NewRecorder()

	h.MintToken(rec, nodeEnrollMintReq("u1", map[string]interface{}{}))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if len(fs.createCalls) != 0 {
		t.Fatalf("expected no CreateNodeEnrollToken call, got %+v", fs.createCalls)
	}
}

func TestMintToken_UnauthorizedWhenNoCaller(t *testing.T) {
	fs := &nodeEnrollFakeStore{}
	state := newNodeEnrollState(fs, true, false, "")
	h := NewNodeEnrollHandler(state)
	rec := httptest.NewRecorder()

	h.MintToken(rec, nodeEnrollMintReq("", map[string]interface{}{}))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if len(fs.createCalls) != 0 {
		t.Fatalf("expected no CreateNodeEnrollToken call, got %+v", fs.createCalls)
	}
}

// TestMintToken_ExpiryClamp pins the exact clamp boundaries from
// node_enroll.go: days<=0 -> 7, days>30 -> 30, and the cap itself (30) is a
// valid pass-through value (not >, so it is not re-clamped).
func TestMintToken_ExpiryClamp(t *testing.T) {
	cases := []struct {
		name     string
		days     int
		wantDays int
	}{
		{"zero clamps to the 7-day default", 0, 7},
		{"negative clamps to the 7-day default", -5, 7},
		{"1 day passes through unchanged", 1, 1},
		{"exactly at the 30-day cap passes through unchanged", 30, 30},
		{"31 (just over the cap) clamps down to 30", 31, 30},
		{"999 clamps down to the 30-day cap", 999, 30},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &nodeEnrollFakeStore{}
			state := newNodeEnrollState(fs, true, false, "")
			h := NewNodeEnrollHandler(state)
			rec := httptest.NewRecorder()

			before := time.Now()
			h.MintToken(rec, nodeEnrollMintReq("u1", map[string]interface{}{"expiresDays": c.days}))
			after := time.Now()

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if len(fs.createCalls) != 1 {
				t.Fatalf("expected 1 CreateNodeEnrollToken call, got %d", len(fs.createCalls))
			}
			call := fs.createCalls[0]
			if call.expiresAt == nil {
				t.Fatalf("expiresAt is nil, want a clamped expiry")
			}
			wantLower := before.AddDate(0, 0, c.wantDays).Add(-2 * time.Second)
			wantUpper := after.AddDate(0, 0, c.wantDays).Add(2 * time.Second)
			if call.expiresAt.Before(wantLower) || call.expiresAt.After(wantUpper) {
				t.Fatalf("expiresAt = %v, want within [%v, %v] (now+%d days)", call.expiresAt, wantLower, wantUpper, c.wantDays)
			}
		})
	}
}

// TestMintToken_FingerprintDisclosureGate pins that the response only ever
// carries grpcTlsFingerprint when GRPCTLSEnabled is true - showing pinning
// material the control channel does not actually enforce would be
// misleading.
func TestMintToken_FingerprintDisclosureGate(t *testing.T) {
	cases := []struct {
		name           string
		grpcTLSEnabled bool
		fingerprint    string
		wantInBody     string
	}{
		{"TLS disabled: fingerprint withheld even if one is configured", false, "aa:bb:cc", ""},
		{"TLS enabled: configured fingerprint is disclosed", true, "aa:bb:cc", "aa:bb:cc"},
		{"TLS enabled but no fingerprint derived: empty string disclosed", true, "", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &nodeEnrollFakeStore{}
			state := newNodeEnrollState(fs, true, c.grpcTLSEnabled, c.fingerprint)
			h := NewNodeEnrollHandler(state)
			rec := httptest.NewRecorder()

			h.MintToken(rec, nodeEnrollMintReq("u1", map[string]interface{}{}))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				GRPCTLSFingerprint string `json:"grpcTlsFingerprint"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.GRPCTLSFingerprint != c.wantInBody {
				t.Fatalf("grpcTlsFingerprint = %q, want %q", resp.GRPCTLSFingerprint, c.wantInBody)
			}
		})
	}
}

// TestMintToken_BodyHandling pins the two halves of the decode contract, which
// have to be distinguished rather than collapsed: BOTH fields are optional, so
// an empty body legitimately means "mint one with the defaults" (io.EOF), while
// anything else is malformed and must not mint.
//
// This previously documented the opposite - the decode error was discarded
// entirely, so a caller who sent `{"expiresDays": 30}` with a typo elsewhere in
// the JSON got a silent 7-day token and no way to tell. Rejecting outright
// would have been just as wrong, because it would break the no-body call.
func TestMintToken_BodyHandling(t *testing.T) {
	tests := []struct {
		name        string
		body        []byte
		wantStatus  int
		wantCreates int
	}{
		{"malformed", []byte("not json"), http.StatusBadRequest, 0},
		{"truncated", []byte(`{"label":`), http.StatusBadRequest, 0},
		{"empty body means defaults", nil, http.StatusOK, 1},
		{"explicit empty object", []byte(`{}`), http.StatusOK, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &nodeEnrollFakeStore{}
			state := newNodeEnrollState(fs, true, false, "")
			h := NewNodeEnrollHandler(state)
			r := httptest.NewRequest("POST", "/api/nodes/enroll-token", bytes.NewReader(tt.body))
			r = r.WithContext(context.WithValue(r.Context(), "userID", "u1"))
			rec := httptest.NewRecorder()

			h.MintToken(rec, r)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if len(fs.createCalls) != tt.wantCreates {
				t.Fatalf("CreateNodeEnrollToken calls = %d, want %d", len(fs.createCalls), tt.wantCreates)
			}
			// A rejected body must not mint anything - the whole point of the
			// 400 is that no credential is issued for input we could not read.
			if tt.wantStatus == http.StatusOK && fs.createCalls[0].label != "" {
				t.Fatalf("label = %q, want empty (zero-value request)", fs.createCalls[0].label)
			}
		})
	}
}

func TestMintToken_StoreErrorIsInternalServerError(t *testing.T) {
	fs := &nodeEnrollFakeStore{createErr: errors.New("db down")}
	state := newNodeEnrollState(fs, true, false, "")
	h := NewNodeEnrollHandler(state)
	rec := httptest.NewRecorder()

	h.MintToken(rec, nodeEnrollMintReq("u1", map[string]interface{}{}))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
}

// TestMintToken_HonorsTheNodeCap pins that minting an enroll token is capped the
// way the other two tenant-facing mints are.
//
// MintNodeWarpKey counts unrevoked warp keys against max_nodes, MintLinkKit
// counts link kits against max_links; this endpoint - the third door to the same
// outcome, an owned node - counted nothing.
//
// The limit was never bypassable (Handshake.Enroll checks NodeLimitReached
// before creating the node), but it refused at the far end of the flow, and the
// gRPC layer flattens every enrollment error to "enrollment failed". A tenant
// over their plan set a machine up, watched it fail to pair, and had nothing
// telling them why. A redeemable token is a pending node, which is why it counts
// - the same reasoning behind the warp sibling's "Revoke an unused key or remove
// a machine first".
func TestMintToken_HonorsTheNodeCap(t *testing.T) {
	cap2 := func() *store.UserBilling {
		n := int64(2)
		return &store.UserBilling{Status: "active", MaxNodes: &n}
	}

	tests := []struct {
		name          string
		billing       *store.UserBilling
		nodes         int
		pendingTokens int
		warpNodeKeys  int
		wantStatus    int
	}{
		// Self-host, nothing configured: no cap. Reached with StoreEnabled false
		// below, because on a HOSTED install this same state means "bought
		// nothing" and is refused by the entitlement gate instead.
		{name: "nothing configured means no cap", wantStatus: http.StatusOK},
		{name: "under the cap", billing: cap2(), nodes: 1, wantStatus: http.StatusOK},
		{
			name:    "machines alone reach the cap",
			billing: cap2(), nodes: 2, wantStatus: http.StatusForbidden,
		},
		{
			// The case the sibling endpoint already refuses: nothing is enrolled
			// yet, but every slot is spoken for by a token that can still be
			// redeemed. Counting only machines would hand out a token that is
			// guaranteed to fail at pairing time.
			name:    "unredeemed tokens fill the remaining slots",
			billing: cap2(), nodes: 1, pendingTokens: 1, wantStatus: http.StatusForbidden,
		},
		{
			// The cross-door case, and the one this gate got backwards. A warp
			// key with no token is a machine MID-SETUP: the panel mints the key
			// and then this token for the same machine the user named once, so
			// refusing here made the machine unaddable rather than capping
			// anything. On max_nodes = 1 - a manual grant, or a one-unit
			// purchase - the FIRST machine could never be added at all.
			name:    "the token completing a machine the warp key started",
			billing: cap2(), nodes: 1, warpNodeKeys: 1, wantStatus: http.StatusOK,
		},
		{
			// And that is as far as it goes: with both halves already out, a
			// second token is a THIRD machine on a cap of two.
			name:    "a second token once that machine has both halves",
			billing: cap2(), nodes: 1, warpNodeKeys: 1, pendingTokens: 1, wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &nodeEnrollFakeStore{
				billing:       tt.billing,
				nodes:         tt.nodes,
				pendingTokens: tt.pendingTokens,
				warpNodeKeys:  tt.warpNodeKeys,
			}
			h := &NodeEnrollHandler{state: newNodeEnrollState(fs, true, false, "")}

			req := httptest.NewRequest(http.MethodPost, "/api/nodes/enroll-token", nil)
			req = req.WithContext(context.WithValue(req.Context(), "userID", "u1"))
			rec := httptest.NewRecorder()
			h.MintToken(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusForbidden {
				if len(fs.createCalls) != 0 {
					t.Error("a refused mint still wrote a token row")
				}
				if !strings.Contains(rec.Body.String(), "Node limit reached") {
					t.Errorf("body = %q, want the same wording the warp key mint uses", rec.Body.String())
				}
			} else if len(fs.createCalls) != 1 {
				t.Errorf("createCalls = %d, want 1", len(fs.createCalls))
			}
		})
	}
}

// warpKeyNodeId names the node key minted for the same machine, so redeeming the
// token binds it to the node. Only the caller's own, live, unbound NODE key may
// be named: a route-only key would tie a link kit to a node, a foreign key would
// let a tenant claim someone else's overlay identity for their machine, and a
// bound one already answers link-boot for another machine.
func TestMintToken_WarpKeyNodeIdMustBeAnOwnFreeNodeKey(t *testing.T) {
	revokedAt := time.Now()
	keys := map[string]*store.WarpAPIKey{
		"node-mine":    {ID: 1, NodeID: "node-mine", OwnerID: "u1"},
		"node-foreign": {ID: 2, NodeID: "node-foreign", OwnerID: "u2"},
		"node-bound":   {ID: 3, NodeID: "node-bound", OwnerID: "u1", BoundNodeID: 9},
		"node-revoked": {ID: 4, NodeID: "node-revoked", OwnerID: "u1", RevokedAt: &revokedAt},
		"link-mine":    {ID: 5, NodeID: "link-mine", OwnerID: "u1"},
	}
	tests := []struct {
		name       string
		keyID      string
		wantStatus int
	}{
		{"no key named", "", http.StatusOK},
		{"an own free node key", "node-mine", http.StatusOK},
		{"someone else's node key", "node-foreign", http.StatusBadRequest},
		{"an unknown key", "node-unknown", http.StatusBadRequest},
		{"a route-only key", "link-mine", http.StatusBadRequest},
		{"a key already bound to a machine", "node-bound", http.StatusBadRequest},
		{"a revoked key", "node-revoked", http.StatusBadRequest},
	}
	bodies := map[string]string{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &nodeEnrollFakeStore{warpKeys: keys}
			h := NewNodeEnrollHandler(newNodeEnrollState(fs, true, false, ""))
			rec := httptest.NewRecorder()

			h.MintToken(rec, nodeEnrollMintReq("u1", map[string]interface{}{"warpKeyNodeId": tt.keyID}))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			bodies[tt.keyID] = rec.Body.String()
			if tt.wantStatus != http.StatusOK {
				if len(fs.createCalls) != 0 {
					t.Error("a refused key still minted a token")
				}
				return
			}
			if len(fs.createCalls) != 1 || fs.createCalls[0].warpKeyNodeID != tt.keyID {
				t.Fatalf("createCalls = %+v, want one carrying %q", fs.createCalls, tt.keyID)
			}
		})
	}
	// Someone else's key must read exactly like one that does not exist.
	if bodies["node-foreign"] != bodies["node-unknown"] {
		t.Errorf("a foreign key answers %q, an unknown one %q: the difference confirms it exists",
			bodies["node-foreign"], bodies["node-unknown"])
	}
}
