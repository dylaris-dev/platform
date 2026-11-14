package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/handlers/auth"
	"dylaris-core/models"
	"dylaris-core/services"
)

// keyedAuthzStore answers every key lookup with one key.
type keyedAuthzStore struct {
	*authzFakeStore
	key *models.APIKey
}

func (f *keyedAuthzStore) GetAPIKeyByHash(string) (*models.APIKey, error) { return f.key, nil }

// TestExternalFloodgateKeyNeedsWriteScope drives the REAL router: a read-only
// key is what a customer hands a dashboard, and it must not fetch a
// credential. A write key passes the scope check (whatever the handler then
// answers).
func TestExternalFloodgateKeyNeedsWriteScope(t *testing.T) {
	const denied = "Key lacks required permission"
	for _, c := range []struct {
		perms      []string
		wantDenied bool
	}{
		{[]string{"gateway.read"}, true},
		{[]string{"usage.read"}, true},
		{[]string{"gateway.write"}, false},
	} {
		fs := &authzFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
		owner := fs.addUser("owner-id", "owner", false)
		st := &keyedAuthzStore{authzFakeStore: fs, key: &models.APIKey{ID: 1, UserID: owner.ID, RatePerMin: 100,
			Scope: models.APIKeyScope{Permissions: c.perms}}}
		appState := &handlers.AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Authz: authz.NewResolver(st),
			GatewayRemote: func() (bool, bool) { return false, true }}
		authHandler := auth.NewAuthHandler(authDepsOf(appState), testJWTSecret)
		root, _ := buildAPIRouter(appState, authHandler, routeCfg{JWTSecret: testJWTSecret})

		req := httptest.NewRequest("GET", "/api/external/gateway/routes/a.eu.example.com/floodgate-key", nil)
		req.Header.Set("Authorization", "Bearer dyl_k")
		rec := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }() // past the scope check the fake store may run out
			root.ServeHTTP(rec, req)
		}()
		var body struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if got := rec.Code == http.StatusForbidden && body.Message == denied; got != c.wantDenied {
			t.Errorf("%v: %d %q, want denied=%v", c.perms, rec.Code, body.Message, c.wantDenied)
		}
		if rec.Code == http.StatusNotFound && body.Message == "" {
			t.Errorf("%v: the route is not registered", c.perms)
		}
	}
}
