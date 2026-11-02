package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
)

// The operator gate: whether NON-ADMINS may hold API keys at all, and which
// capabilities they may put on one. Both halves default to closed, because a
// key is a second credential class with a life of its own - it outlives a
// session, it is not covered by the account's 2FA, and it is revoked
// separately. A fresh install should not start handing those out just because
// the software supports them.

func gateHandler(t *testing.T, enabled string, allowedCaps string, fs *apiKeysAuthFakeStore) *APIKeysHandler {
	t.Helper()
	fs.settings = map[string]string{"apikeys_user_enabled": enabled}
	if allowedCaps != "" {
		fs.settings["apikeys_user_allowed_caps"] = allowedCaps
	}
	return newAPIKeysAuthHandler(fs)
}

func TestAPIKeysCreate_OperatorGateBlocksNonAdmins(t *testing.T) {
	fs := &apiKeysAuthFakeStore{}
	h := gateHandler(t, "false", "", fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("u1", "alice", false, map[string]interface{}{
		"name": "k", "permissions": []string{"modpack.read"},
	}))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if len(fs.createCalls) != 0 {
		t.Fatalf("a key was minted while the feature is off: %+v", fs.createCalls)
	}
}

// An admin is not gated: the switch is about handing the credential class to
// users, and an operator who cannot mint their own key could not turn the
// feature on for anyone either.
func TestAPIKeysCreate_OperatorGateDoesNotBlockAdmins(t *testing.T) {
	// Minting re-authenticates now (requireReauth), so this has to present the
	// password as well - the assertion is that the OPERATOR gate lets an admin
	// through, not that minting asks for nothing.
	fs := &apiKeysAuthFakeStore{users: map[string]*models.User{
		"admin-1": {ID: "admin-1", Password: hashFor(t, "admin-pw")},
	}}
	h := gateHandler(t, "false", "", fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("admin-1", "root", true, map[string]interface{}{
		"name": "k", "permissions": []string{"modpack.read"}, "password": "admin-pw",
	}))

	if rec.Code == http.StatusForbidden {
		t.Fatalf("an admin was blocked by the user gate: %s", rec.Body.String())
	}
	if len(fs.createCalls) != 1 {
		t.Fatalf("expected the admin's key to be minted, got %d calls", len(fs.createCalls))
	}
}

func TestAPIKeysCreate_OperatorCapWhitelist(t *testing.T) {
	t.Run("a capability outside the whitelist is refused", func(t *testing.T) {
		fs := &apiKeysAuthFakeStore{}
		h := gateHandler(t, "true", "modpack.read", fs)
		rec := httptest.NewRecorder()

		h.Create(rec, createAPIKeyReq("u1", "alice", false, map[string]interface{}{
			"name": "k", "permissions": []string{"modpack.write"},
		}))

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "modpack.write") {
			t.Errorf("body = %s, want it to name the refused capability", rec.Body.String())
		}
	})

	t.Run("a capability inside the whitelist passes the gate", func(t *testing.T) {
		fs := &apiKeysAuthFakeStore{}
		h := gateHandler(t, "true", "modpack.read,modpack.write", fs)
		rec := httptest.NewRecorder()

		h.Create(rec, createAPIKeyReq("u1", "alice", false, map[string]interface{}{
			"name": "k", "permissions": []string{"modpack.write"},
		}))

		if rec.Code == http.StatusForbidden {
			t.Fatalf("a whitelisted capability was refused: %s", rec.Body.String())
		}
	})

	// The distinction the setting turns on: EMPTY is "the operator expressed no
	// opinion", which must leave the existing delegation subset check in charge.
	// Reading empty as "nothing allowed" would make turning the feature on do
	// nothing, and reading it as "everything allowed" would be the same bug in
	// the dangerous direction.
	t.Run("an unset whitelist adds no restriction", func(t *testing.T) {
		fs := &apiKeysAuthFakeStore{}
		h := gateHandler(t, "true", "", fs)
		rec := httptest.NewRecorder()

		h.Create(rec, createAPIKeyReq("u1", "alice", false, map[string]interface{}{
			"name": "k", "permissions": []string{"modpack.write"},
		}))

		if rec.Code == http.StatusForbidden {
			t.Fatalf("an unset whitelist refused a capability: %s", rec.Body.String())
		}
	})
}

// The half that matters most. Enforcing the switch only when a key is created
// would leave every key minted before it was turned off working afterwards,
// which is not what an operator who turned it off means.
func TestAPIKeyMiddleware_OperatorGateAppliesAtUse(t *testing.T) {
	newFixture := func(ownerIsAdmin bool) *apiKeysAuthFakeStore {
		return &apiKeysAuthFakeStore{
			keysByHash: map[string]*models.APIKey{
				services.HashAPIKey("mintedearlier"): {
					ID: 1, RatePerMin: 1000, UserID: "owner-1",
					Scope: models.APIKeyScope{Permissions: []string{"modpack.read"}},
				},
			},
			users: map[string]*models.User{
				"owner-1": {ID: "owner-1", Username: "owner", IsAdmin: ownerIsAdmin},
			},
		}
	}
	call := func(h *APIKeysHandler) *httptest.ResponseRecorder {
		wrapped := h.APIKeyOwnerRoute("modpack.read")(sentinelInner)
		r := httptest.NewRequest("GET", "/api/external/library", nil)
		r.Header.Set("Authorization", "Bearer mintedearlier")
		rec := httptest.NewRecorder()
		wrapped(rec, r)
		return rec
	}

	t.Run("a key minted earlier stops working once the feature is off", func(t *testing.T) {
		rec := call(gateHandler(t, "false", "", newFixture(false)))
		if rec.Code == sentinelStatus {
			t.Fatal("the inner handler was reached: turning the feature off must stop existing keys, not only new ones")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the same key works while the feature is on", func(t *testing.T) {
		rec := call(gateHandler(t, "true", "", newFixture(false)))
		if rec.Code != sentinelStatus {
			t.Fatalf("status = %d, want the inner handler reached: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an admin's key is unaffected by the user gate", func(t *testing.T) {
		rec := call(gateHandler(t, "false", "", newFixture(true)))
		if rec.Code != sentinelStatus {
			t.Fatalf("status = %d, want the inner handler reached for an admin-owned key: %s", rec.Code, rec.Body.String())
		}
	})
}
