package handlers

import (
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

// externalMintStore records what MintExternalNodeKey writes. Embeds store.Store
// (nil), so any call the handler is not supposed to make panics.
type externalMintStore struct {
	store.Store
	settings map[string]string

	keys      []store.WarpAPIKey
	deleted   []int
	tokens    []externalMintToken
	tokenErr  error
	nextKeyID int
}

type externalMintToken struct {
	minter, plaintext, label, keyNodeID string
	expiresAt                           *time.Time
}

func (f *externalMintStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *externalMintStore) CreateWarpAPIKey(k store.WarpAPIKey) (int, error) {
	f.nextKeyID++
	f.keys = append(f.keys, k)
	return f.nextKeyID, nil
}
func (f *externalMintStore) CreatePlatformNodeEnrollToken(minter, plaintext, label string, exp *time.Time, keyNodeID string) error {
	if f.tokenErr != nil {
		return f.tokenErr
	}
	f.tokens = append(f.tokens, externalMintToken{minter, plaintext, label, keyNodeID, exp})
	return nil
}
func (f *externalMintStore) DeleteWarpAPIKeyByID(id int) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// newExternalMintHandler: gateway routing on and BYON OFF, because the External
// node mint must not depend on the tenant feature.
func newExternalMintHandler() (*WarpHandler, *externalMintStore) {
	fs := &externalMintStore{settings: map[string]string{"routing_mode": "gateway", "feature_byon_enabled": "false"}}
	state := &AppState{
		Store:              fs,
		FeatureFlags:       services.NewFeatureFlags(fs),
		Gateway:            services.NewRedisGateway(nil, fs, "cs"),
		GRPCTLSEnabled:     true,
		GRPCTLSFingerprint: "ab:cd",
	}
	return NewWarpHandler(state, nil), fs
}

func mintExternal(h *WarpHandler, userID, body string) *httptest.ResponseRecorder {
	return mintExternalAs(h, userID, true, body)
}

func mintExternalAs(h *WarpHandler, userID string, admin bool, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/admin/warp/external-nodes", strings.NewReader(body))
	ctx := context.WithValue(r.Context(), "isAdmin", admin)
	if userID != "" {
		ctx = context.WithValue(ctx, "userID", userID)
	}
	r = r.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.MintExternalNodeKey(rec, r)
	return rec
}

// topology.write can be delegated to someone who is not an admin, and an
// unowned node receives other customers' servers through automatic placement.
// So the capability at the route is not enough: the handler asks for an admin.
func TestMintExternalNodeKey_RefusesACallerWhoIsNotAnAdmin(t *testing.T) {
	h, fs := newExternalMintHandler()

	rec := mintExternalAs(h, "delegate-1", false, `{"name":"ext-frankfurt"}`)

	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Only an admin can add an External node") {
		t.Fatalf("status = %d (%s), want 403 naming the admin requirement", rec.Code, rec.Body.String())
	}
	if len(fs.keys) != 0 || len(fs.tokens) != 0 {
		t.Errorf("a refused mint wrote keys = %d, tokens = %d", len(fs.keys), len(fs.tokens))
	}
}

// One call gives the machine everything: an owner-less node- key in the tenant
// shape (one machine, kill_old) and a platform token bound to that key. With
// BYON off - the machine is nobody's, so the tenant feature has no say.
func TestMintExternalNodeKey_MintsAnOwnerlessKeyAndItsPlatformToken(t *testing.T) {
	h, fs := newExternalMintHandler()

	rec := mintExternal(h, "admin-1", `{"name":"ext-frankfurt"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.keys) != 1 || len(fs.tokens) != 1 {
		t.Fatalf("keys = %d, tokens = %d, want 1 and 1", len(fs.keys), len(fs.tokens))
	}
	k, tok := fs.keys[0], fs.tokens[0]
	if k.OwnerID != "" {
		t.Errorf("key owner = %q, want none", k.OwnerID)
	}
	if !strings.HasPrefix(k.NodeID, "node-") || k.Policy != "general" || k.MaxConns != 1 || k.OnNewConn != "kill_old" {
		t.Errorf("key = %+v, want a node- identity, general, max 1, kill_old", k)
	}
	if tok.keyNodeID != k.NodeID || tok.minter != "admin-1" || tok.label != "ext-frankfurt" {
		t.Errorf("token = %+v, want it bound to %s and minted by admin-1", tok, k.NodeID)
	}
	if tok.expiresAt == nil || tok.expiresAt.Before(time.Now().AddDate(0, 0, 6)) || tok.expiresAt.After(time.Now().AddDate(0, 0, 8)) {
		t.Errorf("token expiry = %v, want 7 days", tok.expiresAt)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["node_id"] != k.NodeID || got["enroll_token"] != tok.plaintext || got["grpc_tls_fingerprint"] != "ab:cd" {
		t.Errorf("answer = %v, want the key's node id, the token and the fingerprint", got)
	}
	if w, _ := got["warp_key"].(string); w == "" || HashAPIKey(w) != k.KeyHash {
		t.Errorf("warp_key does not match the stored hash")
	}
}

func TestMintExternalNodeKey_Refusals(t *testing.T) {
	tests := []struct {
		name       string
		routing    string
		user       string
		body       string
		wantStatus int
	}{
		{name: "gateway routing off", routing: "ip_port", user: "admin-1", body: `{"name":"ext-frankfurt"}`, wantStatus: http.StatusConflict},
		{name: "no caller", routing: "gateway", body: `{"name":"ext-frankfurt"}`, wantStatus: http.StatusUnauthorized},
		{name: "a name that cannot be a NODE_ID", routing: "gateway", user: "admin-1", body: `{"name":"My Home PC"}`, wantStatus: http.StatusBadRequest},
		{name: "no name", routing: "gateway", user: "admin-1", body: `{}`, wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, fs := newExternalMintHandler()
			fs.settings["routing_mode"] = tt.routing

			rec := mintExternal(h, tt.user, tt.body)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if len(fs.keys) != 0 || len(fs.tokens) != 0 {
				t.Errorf("a refusal wrote keys = %d, tokens = %d", len(fs.keys), len(fs.tokens))
			}
		})
	}
}

// The token is what makes the key an External node's. Without it the key could
// still join the overlay and would sit in the list as a machine that never
// arrives, so a failed token insert takes the key back out.
func TestMintExternalNodeKey_TokenFailureRemovesTheKey(t *testing.T) {
	h, fs := newExternalMintHandler()
	fs.tokenErr = errors.New("insert failed")

	rec := mintExternal(h, "admin-1", `{"name":"ext-frankfurt"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if len(fs.deleted) != 1 || fs.deleted[0] != 1 {
		t.Errorf("deleted = %v, want the key just created (1)", fs.deleted)
	}
	if strings.Contains(rec.Body.String(), "warp_key") {
		t.Error("a failed mint handed out the key")
	}
}
