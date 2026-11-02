package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
)

// The key middleware stores the key under a context key that handlers/reqctx
// owns, and every handler reads it back through reqctx. If the two ever used
// different key types, every read would miss: the scope checks that run after
// the middleware would see no key, and an operator's key would carry the
// operator's panel override. These requests go through the real middleware
// and the real adapters, and check what the handler behind them sees.
func TestAKeyScopedRequestStaysScopedBehindTheMiddleware(t *testing.T) {
	adminOwner := &models.User{ID: "owner-1", Username: "root", IsAdmin: true}
	servers := map[string]*models.Server{
		"uuid-a": {ID: 10, UUID: "uuid-a", OwnerID: "owner-1"},
		"uuid-b": {ID: 20, UUID: "uuid-b", OwnerID: "owner-1"},
	}

	type seen struct {
		reached, serverAllowed, isAdmin, override bool
		key                                       *models.APIKey
		allowed                                   []string
	}
	serverRoute := func(t *testing.T, uuid string) (*httptest.ResponseRecorder, seen) {
		t.Helper()
		h := newAPIKeysAuthHandler(externalFixture([]string{"console.send"}, []string{"uuid-a"}, adminOwner, servers))
		var got seen
		probe := func(w http.ResponseWriter, r *http.Request) {
			got = seen{
				reached:       true,
				key:           reqctx.APIKeyFromContext(r),
				serverAllowed: reqctx.APIKeyServerAllowed(r),
				isAdmin:       reqctx.IsAdmin(r),
				override:      reqctx.OperatorOverride(r),
				allowed:       APIKeyAllowedServers(r),
			}
			w.WriteHeader(sentinelStatus)
		}
		wrapped := h.APIKeyServerRoute("console.send")(h.ExternalServerRoute(probe))
		rec := httptest.NewRecorder()
		wrapped(rec, externalRequest("POST", "/api/external/servers/"+uuid+"/console/command",
			map[string]string{"uuid": uuid}, `{"command":"list"}`))
		return rec, got
	}

	t.Run("a server outside the key's scope is refused", func(t *testing.T) {
		rec, got := serverRoute(t, "uuid-b")
		if got.reached {
			t.Fatal("the handler was reached for uuid-b: a key scoped to uuid-a acted on another server")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the handler behind an allowed server still sees the key", func(t *testing.T) {
		rec, got := serverRoute(t, "uuid-a")
		if !got.reached {
			t.Fatalf("status = %d, want the handler reached: %s", rec.Code, rec.Body.String())
		}
		if got.key == nil || got.key.ID != 1 {
			t.Fatalf("APIKeyFromContext = %v, want the key: the handler sees a session, not a scoped key", got.key)
		}
		if !got.serverAllowed {
			t.Error("APIKeyServerAllowed = false on the server the key is scoped to")
		}
		if len(got.allowed) != 1 || got.allowed[0] != "uuid-a" {
			t.Errorf("APIKeyAllowedServers = %v, want [uuid-a]", got.allowed)
		}
		// keyOwnerContext gives the handler the owner's identity, an admin here
		if !got.isAdmin {
			t.Fatal("IsAdmin = false: the probe no longer tests what it claims")
		}
		if got.override {
			t.Error("OperatorOverride = true for an admin's API key: the key carries the panel bypass")
		}
	})

	t.Run("an owner-shaped listing returns only the key's servers", func(t *testing.T) {
		fs := externalFixture(nil, []string{"uuid-a"}, adminOwner, servers)
		fs.fleetServers = []models.Server{*servers["uuid-a"], *servers["uuid-b"]}
		h := newAPIKeysAuthHandler(fs)
		rec := httptest.NewRecorder()
		h.APIKeyOwnerRoute("")(h.ListExternalServers)(rec, externalRequest("GET", "/api/external/servers", nil, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "uuid-a") || strings.Contains(body, "uuid-b") {
			t.Errorf("body = %s, want uuid-a and not uuid-b", body)
		}
	})
}
