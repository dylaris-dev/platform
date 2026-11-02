package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
)

// The route shape used to be inferred from whether the path happened to carry a
// {uuid}, and both guards switched themselves OFF when it did not:
//
//	serverAllowed := uuidVar == "" || key.Scope.AllowsServer(uuidVar)
//	func ownerStillHolds(...) { if serverUUID == "" { return true } }
//
// These tests pin the two halves of the replacement. The server-shape cases
// live in the table in api_keys_auth_test.go; this file covers the owner shape
// and the context the handlers need in order to scope themselves.

// An OWNER capability is the whole point of an owner-scoped route, so it has to
// resolve there.
func TestAPIKeyOwnerRoute_HonorsAnOwnerCapability(t *testing.T) {
	key := &models.APIKey{
		ID: 1, RatePerMin: 1000, UserID: "owner-1",
		Scope: models.APIKeyScope{Permissions: []string{"modpack.read"}},
	}
	fs := &apiKeysAuthFakeStore{
		keysByHash: map[string]*models.APIKey{services.HashAPIKey("ownercap"): key},
		users:      map[string]*models.User{"owner-1": {ID: "owner-1", Username: "owner"}},
	}
	h := newAPIKeysAuthHandler(fs)
	wrapped := h.APIKeyOwnerRoute("modpack.read")(sentinelInner)

	r := httptest.NewRequest("GET", "/api/external/library", nil)
	r.Header.Set("Authorization", "Bearer ownercap")
	rec := httptest.NewRecorder()
	wrapped(rec, r)

	if rec.Code != sentinelStatus {
		t.Fatalf("status = %d, want the inner handler reached (%d): %s", rec.Code, sentinelStatus, rec.Body.String())
	}
}

// The re-check used to return true the moment there was no server in the path,
// so on an owner-shaped route it did not run at all: a key kept working after
// the account behind it was gone. This is the case that proves it runs now.
//
// Worth stating plainly what it does NOT catch: Resolve(id, 0) sets ownerSelf,
// so an existing owner always holds their own OWNER capabilities. On this shape
// the re-check is a liveness check on the account, not a permission diff. The
// permission diff is real on the server shape, which has its own test.
func TestAPIKeyOwnerRoute_RechecksThatTheOwnerStillExists(t *testing.T) {
	key := &models.APIKey{
		ID: 2, RatePerMin: 1000, UserID: "ghost",
		Scope: models.APIKeyScope{Permissions: []string{"modpack.read"}},
	}
	fs := &apiKeysAuthFakeStore{
		keysByHash: map[string]*models.APIKey{services.HashAPIKey("ghostkey"): key},
		users:      map[string]*models.User{}, // the owner's account is gone
	}
	h := newAPIKeysAuthHandler(fs)
	wrapped := h.APIKeyOwnerRoute("modpack.read")(sentinelInner)

	r := httptest.NewRequest("GET", "/api/external/library", nil)
	r.Header.Set("Authorization", "Bearer ghostkey")
	rec := httptest.NewRecorder()
	wrapped(rec, r)

	if rec.Code == sentinelStatus {
		t.Fatal("the inner handler was reached: a key whose owner no longer exists must be refused")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no longer valid") {
		t.Errorf("body = %s, want it to name the reason", rec.Body.String())
	}
}

// An owner-scoped handler cannot scope itself without knowing whose realm it is
// acting on and which servers the key was minted for. The middleware's doc
// comment claimed it "injects the resolved APIKey into the context" while no
// such injection existed anywhere in the file - so a handler asking for it got
// nothing, silently, and the natural fix would have been to scope by owner
// alone, which is wider than the key's allowlist.
func TestAPIKeyMiddleware_InjectsTheKeyForHandlerScoping(t *testing.T) {
	key := &models.APIKey{
		ID: 3, RatePerMin: 1000, UserID: "owner-7",
		Scope: models.APIKeyScope{Permissions: []string{"modpack.read"}, Servers: []string{"srv-a", "srv-b"}},
	}
	fs := &apiKeysAuthFakeStore{
		keysByHash: map[string]*models.APIKey{services.HashAPIKey("ctxkey"): key},
		users:      map[string]*models.User{"owner-7": {ID: "owner-7", Username: "owner"}},
	}
	h := newAPIKeysAuthHandler(fs)

	var gotCaller string
	var gotServers []string
	inner := func(w http.ResponseWriter, r *http.Request) {
		gotCaller = APIKeyCallerID(r)
		gotServers = APIKeyAllowedServers(r)
		w.WriteHeader(http.StatusOK)
	}
	wrapped := h.APIKeyOwnerRoute("modpack.read")(inner)

	r := httptest.NewRequest("GET", "/api/external/library", nil)
	r.Header.Set("Authorization", "Bearer ctxkey")
	wrapped(httptest.NewRecorder(), r)

	if gotCaller != "owner-7" {
		t.Errorf("APIKeyCallerID = %q, want the key's owner. Without it a handler cannot bind its query to one realm", gotCaller)
	}
	if len(gotServers) != 2 || gotServers[0] != "srv-a" || gotServers[1] != "srv-b" {
		t.Errorf("APIKeyAllowedServers = %v, want the key's allowlist. Without it a listing route would return every server the owner has", gotServers)
	}
}

// A request with no key must not report one, or a handler that scopes by
// APIKeyCallerID would scope to the empty string instead of failing.
func TestAPIKeyContextHelpersAreEmptyWithoutAKey(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/external/library", nil)
	if id := APIKeyCallerID(r); id != "" {
		t.Errorf("APIKeyCallerID = %q, want empty", id)
	}
	if s := APIKeyAllowedServers(r); s != nil {
		t.Errorf("APIKeyAllowedServers = %v, want nil", s)
	}
}
