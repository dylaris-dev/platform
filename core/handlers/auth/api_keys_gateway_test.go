package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"dylaris-core/authz"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// rollFakeStore adds what the managed-gateway key work needs on top of the
// shared fake: an entitlement row and a roll that mirrors the real store's
// contract (one previous hash, valid until its expiry, dead with its key).
type rollFakeStore struct {
	*apiKeysAuthFakeStore
	billing *store.UserBilling
	prev    map[string]prevHash
	now     func() time.Time
}

type prevHash struct {
	keyID int
	exp   time.Time
}

func (f *rollFakeStore) GetUserBilling(string) (*store.UserBilling, error) { return f.billing, nil }

func (f *rollFakeStore) GetAPIKeyByHash(hash string) (*models.APIKey, error) {
	if k, err := f.apiKeysAuthFakeStore.GetAPIKeyByHash(hash); err == nil {
		return k, nil
	}
	p, ok := f.prev[hash]
	if !ok || !p.exp.After(f.now()) {
		return nil, errors.New("not found")
	}
	for _, k := range f.keysByHash {
		if k.ID == p.keyID && k.RevokedAt == nil && (k.ExpiresAt == nil || k.ExpiresAt.After(f.now())) {
			c := *k
			c.ViaPrevious = true
			return &c, nil
		}
	}
	return nil, errors.New("not found")
}

func (f *rollFakeStore) RollAPIKey(_ context.Context, id int, ownerID, expectHash, newHash string, grace time.Duration) error {
	for h, k := range f.keysByHash {
		if k.ID == id {
			if k.UserID != ownerID || k.KeyHash != expectHash {
				return store.ErrAPIKeyRollConflict
			}
			delete(f.keysByHash, h)
			f.keysByHash[newHash] = k
			k.KeyHash = newHash
			f.prev = map[string]prevHash{h: {keyID: id, exp: f.now().Add(grace)}}
			return nil
		}
	}
	return errors.New("not found")
}

const gwOwner = "owner-1"

// newGatewayHandler: user keys OFF, route-only product on, store plane on. The
// owner holds a route-only purchase (MaxLinks 1) when entitled.
func newGatewayHandler(entitled bool, extraSettings map[string]string) (*APIKeysHandler, *rollFakeStore) {
	return newGatewayHandlerStore(entitled, true, extraSettings)
}

func newGatewayHandlerStore(entitled, storeEnabled bool, extraSettings map[string]string) (*APIKeysHandler, *rollFakeStore) {
	fs := &apiKeysAuthFakeStore{
		settings:   map[string]string{"apikeys_user_enabled": "false", "feature_route_only_enabled": "true"},
		users:      map[string]*models.User{gwOwner: {ID: gwOwner, Username: "o", Password: reauthTestHash}},
		keysByHash: map[string]*models.APIKey{},
	}
	for k, v := range extraSettings {
		fs.settings[k] = v
	}
	rs := &rollFakeStore{apiKeysAuthFakeStore: fs, now: time.Now}
	if entitled {
		one := int64(1)
		rs.billing = &store.UserBilling{UserID: gwOwner, Status: "active", MaxLinks: &one}
	}
	h := NewAPIKeysHandler(Deps{
		Store:        rs,
		Authz:        authz.NewResolver(rs),
		FeatureFlags: services.NewFeatureFlags(rs),
		StoreEnabled: storeEnabled,
	})
	return h, rs
}

func mintStatus(h *APIKeysHandler, perms []string) int {
	rec := httptest.NewRecorder()
	h.Create(rec, createAPIKeyReq(gwOwner, "o", false, map[string]interface{}{"name": "k", "permissions": perms}))
	return rec.Code
}

func TestGatewayKeyMintAllowedWithEntitlement(t *testing.T) {
	h, _ := newGatewayHandler(true, nil)
	// Create answers 200 on success (it never sets another status).
	if got := mintStatus(h, []string{"gateway.read", "gateway.write"}); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
}

func TestGatewayKeyMintRefusedWithoutEntitlement(t *testing.T) {
	h, _ := newGatewayHandler(false, nil)
	rec := httptest.NewRecorder()
	h.Create(rec, createAPIKeyReq(gwOwner, "o", false, map[string]interface{}{"name": "k", "permissions": []string{"gateway.read"}}))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "API keys are not enabled for users on this platform") {
		t.Fatalf("got %d %s, want 403 with the operator-gate message", rec.Code, rec.Body.String())
	}
}

func TestNonGatewayScopeStillNeedsUserKeysOn(t *testing.T) {
	h, _ := newGatewayHandler(true, nil)
	for _, perms := range [][]string{
		{"usage.read", "modpack.read"},
		{"gateway.read", "modpack.read"},
		{"usage.read"}, // no gateway.* present
	} {
		if got := mintStatus(h, perms); got != http.StatusForbidden {
			t.Errorf("%v: status = %d, want 403", perms, got)
		}
	}
}

func TestGatewayKeyMintIgnoresWhitelistButNeedsRouteOnlyOn(t *testing.T) {
	h, _ := newGatewayHandler(true, map[string]string{"apikeys_user_allowed_caps": "usage.read"})
	if got := mintStatus(h, []string{"gateway.write"}); got != http.StatusOK {
		t.Fatalf("whitelist must not block an entitled gateway key, got %d", got)
	}
	h, _ = newGatewayHandler(true, map[string]string{"feature_route_only_enabled": "false"})
	if got := mintStatus(h, []string{"gateway.write"}); got != http.StatusForbidden {
		t.Fatalf("route-only product off must keep the gate, got %d", got)
	}
}

// The use-time half of the rule: a minted gateway key stops when the
// entitlement lapses, with user keys still off.
func TestGatewayKeyStopsWhenEntitlementLapses(t *testing.T) {
	h, rs := newGatewayHandler(true, nil)
	rs.keysByHash[services.HashAPIKey("gk")] = &models.APIKey{ID: 1, UserID: gwOwner, RatePerMin: 100,
		Scope: models.APIKeyScope{Permissions: []string{"gateway.read"}}}
	call := func() int {
		r := httptest.NewRequest("GET", "/x", nil)
		r.Header.Set("Authorization", "Bearer dyl_gk")
		rec := httptest.NewRecorder()
		h.APIKeyOwnerRoute("gateway.read")(sentinelInner)(rec, r)
		return rec.Code
	}
	if got := call(); got != sentinelStatus {
		t.Fatalf("entitled: status = %d, want %d", got, sentinelStatus)
	}
	rs.billing = nil
	if got := call(); got != http.StatusForbidden {
		t.Fatalf("lapsed: status = %d, want 403", got)
	}
}

func rollSelf(h *APIKeysHandler, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/external/api-key/roll", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	h.APIKeyOwnerRoute("")(h.RollSelf)(rec, r)
	return rec
}

func seedKey(rs *rollFakeStore, plain string) {
	rs.keysByHash[services.HashAPIKey(plain)] = &models.APIKey{ID: 7, UserID: gwOwner, RatePerMin: 100,
		KeyHash: services.HashAPIKey(plain), Scope: models.APIKeyScope{Permissions: []string{"gateway.read"}}}
}

func rolledKey(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Success            bool   `json:"success"`
		Key                string `json:"key"`
		ID                 int    `json:"id"`
		PreviousValidUntil string `json:"previousValidUntil"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !out.Success || !strings.HasPrefix(out.Key, "dyl_") || out.ID != 7 {
		t.Fatalf("roll: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := time.Parse(time.RFC3339, out.PreviousValidUntil); err != nil {
		t.Fatalf("previousValidUntil %q: %v", out.PreviousValidUntil, err)
	}
	return out.Key
}

func TestAPIKeyRollKeepsOnePreviousKey(t *testing.T) {
	h, rs := newGatewayHandler(true, nil)
	seedKey(rs, "k0")
	clock := time.Now()
	rs.now = func() time.Time { return clock }

	k1 := rolledKey(t, rollSelf(h, "dyl_k0"))
	k2 := rolledKey(t, rollSelf(h, k1))

	status := func(bearer string) int {
		r := httptest.NewRequest("GET", "/x", nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		h.APIKeyOwnerRoute("gateway.read")(sentinelInner)(rec, r)
		return rec.Code
	}
	if got := status(k2); got != sentinelStatus {
		t.Errorf("newest: %d", got)
	}
	if got := status(k1); got != sentinelStatus {
		t.Errorf("previous: %d, want accepted inside the grace", got)
	}
	if got := status("dyl_k0"); got != http.StatusUnauthorized {
		t.Errorf("oldest: %d, want 401", got)
	}
	// The previous key must not be able to roll: a leaked old key would
	// otherwise lock the legitimate holder out.
	if rec := rollSelf(h, k1); rec.Code != http.StatusForbidden {
		t.Errorf("rolling with the previous key: %d, want 403", rec.Code)
	}
	clock = clock.Add(apiKeyRollGrace + time.Minute)
	if got := status(k1); got != http.StatusUnauthorized {
		t.Errorf("previous after grace: %d, want 401", got)
	}
	if got := status(k2); got != sentinelStatus {
		t.Errorf("newest after grace: %d", got)
	}
}

func TestRollSelfReturnsNewKeyOnce(t *testing.T) {
	h, rs := newGatewayHandler(true, nil)
	seedKey(rs, "k0")
	key := rolledKey(t, rollSelf(h, "dyl_k0"))
	if len(rs.keysByHash) != 1 {
		t.Fatalf("expected the row to move to the new hash, have %d", len(rs.keysByHash))
	}
	if _, ok := rs.keysByHash[services.HashAPIKey(strings.TrimPrefix(key, "dyl_"))]; !ok {
		t.Fatal("new hash is not the hash of the returned key")
	}
	// Only the hash is stored; neither it nor the plaintext is serialised.
	for _, k := range rs.keysByHash {
		if b, _ := json.Marshal(k); strings.Contains(string(b), key) || strings.Contains(string(b), k.KeyHash) {
			t.Fatalf("key row serialises a secret: %s", b)
		}
	}
}

func TestSessionRollIsOwnKeysOnly(t *testing.T) {
	h, rs := newGatewayHandler(true, nil)
	seedKey(rs, "k0")
	rs.listedKeys = []models.APIKey{{ID: 7, UserID: gwOwner, Name: "mine", KeyHash: services.HashAPIKey("k0")}}

	do := func(id, password string) int {
		r := httptest.NewRequest("POST", "/api/me/api-keys/"+id+"/roll", strings.NewReader(`{"password":"`+password+`"}`))
		r = mux.SetURLVars(r.WithContext(context.WithValue(r.Context(), "userID", gwOwner)), map[string]string{"id": id})
		rec := httptest.NewRecorder()
		h.Roll(rec, r)
		return rec.Code
	}
	// The fake lists listedKeys for any user, so the foreign case is an id that
	// is not in the caller's list.
	if got := do("99", reauthTestPassword); got != http.StatusNotFound {
		t.Errorf("foreign/unknown id: %d, want 404", got)
	}
	if got := do("7", "wrong-password"); got == http.StatusOK {
		t.Errorf("wrong password rolled the key")
	}
	if got := do("7", reauthTestPassword); got != http.StatusOK {
		t.Errorf("own key: %d, want 200", got)
	}
}

// Without a store plane EffectiveEntitlement reads "unlimited"; the exemption
// must not follow it, or every self-hosted account could mint gateway keys.
func TestGatewayKeyExemptionNeedsAStore(t *testing.T) {
	h, rs := newGatewayHandlerStore(true, false, nil)
	if got := mintStatus(h, []string{"gateway.read", "gateway.write"}); got != http.StatusForbidden {
		t.Fatalf("mint without a store: %d, want 403", got)
	}
	rs.keysByHash[services.HashAPIKey("gk")] = &models.APIKey{ID: 1, UserID: gwOwner, RatePerMin: 100,
		Scope: models.APIKeyScope{Permissions: []string{"gateway.read", "gateway.write"}}}
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer dyl_gk")
	rec := httptest.NewRecorder()
	h.APIKeyOwnerRoute("gateway.read")(sentinelInner)(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("use without a store: %d, want 403", rec.Code)
	}
}

// Two self-rolls from the same current key: the second sees a stale hash and
// must get 409 instead of replacing the key the first one handed out.
func TestRollSelfLosingTheRaceIs409(t *testing.T) {
	h, rs := newGatewayHandler(true, nil)
	seedKey(rs, "k0")
	stale := *rs.keysByHash[services.HashAPIKey("k0")]
	rolledKey(t, rollSelf(h, "dyl_k0"))

	r := httptest.NewRequest("POST", "/api/external/api-key/roll", nil)
	r = r.WithContext(reqctx.WithAPIKey(r.Context(), &stale, false))
	rec := httptest.NewRecorder()
	h.RollSelf(rec, r)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "The key was rolled meanwhile; use the newest key.") {
		t.Fatalf("got %d %s, want 409 with the roll-meanwhile message", rec.Code, rec.Body.String())
	}
}

func TestGatewayKeyMintRefusesServerScope(t *testing.T) {
	h, _ := newGatewayHandler(true, nil)
	rec := httptest.NewRecorder()
	h.Create(rec, createAPIKeyReq(gwOwner, "o", false, map[string]interface{}{
		"name": "k", "permissions": []string{"gateway.read"}, "servers": []string{"some-uuid"}}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Gateway keys cannot be limited to servers.") {
		t.Fatalf("got %d %s, want 400 with the scope message", rec.Code, rec.Body.String())
	}
}
