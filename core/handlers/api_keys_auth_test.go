package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"golang.org/x/crypto/bcrypt"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// apiKeysAuthFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods the external API-key auth
// chain (apiKeyMiddleware) and Create touch are overridden. Any other call
// would panic - these tests never make one.
type apiKeysAuthFakeStore struct {
	store.Store

	keysByHash map[string]*models.APIKey

	servers      map[string]*models.Server // by UUID, for Create's non-admin scoping check
	getServerErr error

	// grants lets a test hand a caller an explicit per-server grant for the
	// resolver-based delegation subset check in Create. Key: grantKey(serverID, userID).
	grants map[string]*store.ServerGrant

	// users backs the use-time owner re-check in apiKeyMiddleware
	// (ownerStillHolds): the key's authority is re-resolved against what its
	// owner holds NOW. Absent = the owner's account is gone.
	users      map[string]*models.User
	getUserErr error
	// anyUserReauths lets an unseeded caller satisfy Create's requireReauth with
	// reauthTestPassword. Set by newAPIKeysAuthHandler, because the tests below are
	// about the DELEGATION maths and would otherwise each have to restate a
	// credential that is not what they are asserting. The gate itself is covered
	// by reauth_test.go and by TestAPIKeysCreate_RefusesTheWrongAccountPassword,
	// which is what fails if it is ever taken out of Create.
	anyUserReauths bool

	createCalls []*models.APIKey
	createErr   error
	nextID      int

	// settings backs the FeatureFlags service, which the middleware and Create
	// consult for the operator gate on user API keys.
	settings map[string]string

	// backupJobs backs ExternalJobInServer, which checks that a {jobId} really
	// belongs to the {uuid} in the same path. Keyed by job id; see
	// external_api_test.go for the methods that read these.
	backupJobs map[int]*models.BackupJob
	// ownedServers backs ListServersForUser for the external listing route.
	ownedServers []models.Server
	// fleetServers is what ListServersForUser returns for an ADMIN caller, the
	// way the real store answers that branch with the whole fleet rather than a
	// per-owner query. Kept separate so a test can tell the two apart.
	fleetServers []models.Server
}

func (f *apiKeysAuthFakeStore) GetSetting(key string) (string, error) {
	if f.settings == nil {
		return "", nil
	}
	return f.settings[key], nil
}

// grantKey lets a test hand a friend an explicit per-server grant.
// Absent = no grant (deny).
func grantKey(serverID int, userID string) string { return fmt.Sprintf("%d:%s", serverID, userID) }

func (f *apiKeysAuthFakeStore) GetAPIKeyByHash(hash string) (*models.APIKey, error) {
	k, ok := f.keysByHash[hash]
	if !ok {
		return nil, errors.New("not found")
	}
	return k, nil
}

func (f *apiKeysAuthFakeStore) TouchAPIKey(id int) error { return nil }

// The external server list applies the panel's region filter.
func (f *apiKeysAuthFakeStore) GetUserRegionIDs(string) ([]string, error) { return nil, nil }

func (f *apiKeysAuthFakeStore) GetUserByID(id string) (*models.User, error) {
	if f.getUserErr != nil {
		return nil, f.getUserErr
	}
	u, ok := f.users[id]
	if !ok {
		// Only when a test configured NO users at all. A test that builds a users
		// map and leaves an id out of it is saying "this account is gone", which
		// is the middleware's owner re-check - that meaning has to survive.
		if f.anyUserReauths && f.users == nil && id != "" {
			return &models.User{ID: id, Password: reauthTestHash}, nil
		}
		return nil, errors.New("user not found")
	}
	return u, nil
}

func (f *apiKeysAuthFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	if f.getServerErr != nil {
		return nil, f.getServerErr
	}
	srv, ok := f.servers[uuid]
	if !ok {
		return nil, errors.New("server not found")
	}
	return srv, nil
}

// GetInvite: the non-admin scoping check in Create falls through to this
// whenever the caller is not the server owner. None of these tests exercise
// an actual invite grant, so it always reports "no invite" (deny).
func (f *apiKeysAuthFakeStore) GetInvite(serverID int, userID string) (*models.ServerInvite, error) {
	return nil, errors.New("no invite")
}

// The methods below satisfy authz.Store so h.state.Authz.Resolve does not
// panic on the embedded nil store.Store. Deny-by-default except GetServerByID
// (backed by the same `servers` map keyed by UUID, looked up by ID here) and
// GetServerGrant (backed by the configurable `grants` map).
func (f *apiKeysAuthFakeStore) GetUserPanelAuthz(userID string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func (f *apiKeysAuthFakeStore) GetServerByID(id int) (*models.Server, error) {
	for _, s := range f.servers {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, errors.New("server not found")
}

func (f *apiKeysAuthFakeStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	if g, ok := f.grants[grantKey(serverID, userID)]; ok {
		return g, nil
	}
	return nil, nil
}

func (f *apiKeysAuthFakeStore) GetAccountGrant(ownerUserID, userID string) (*store.ServerGrant, error) {
	return nil, nil
}

func (f *apiKeysAuthFakeStore) CreateAPIKey(k *models.APIKey) (int, error) {
	if f.createErr != nil {
		return 0, f.createErr
	}
	f.nextID++
	f.createCalls = append(f.createCalls, k)
	return f.nextID, nil
}

// newAPIKeysAuthHandler builds the handler with user API keys ENABLED, because
// almost every case here is about some other branch. The operator gate itself is
// covered by TestAPIKeyMiddleware_OperatorGate and TestAPIKeysCreate_OperatorGate,
// which set the flag themselves.
// reauthTestPassword is what createAPIKeyReq sends by default, and what an
// unseeded fake user answers to. Cost 4 rather than the real cost: these are
// unit tests, not a password-strength exercise.
const reauthTestPassword = "test-account-password"

var reauthTestHash = func() string {
	h, err := bcrypt.GenerateFromPassword([]byte(reauthTestPassword), 4)
	if err != nil {
		panic(err)
	}
	return string(h)
}()

func newAPIKeysAuthHandler(fs *apiKeysAuthFakeStore) *APIKeysHandler {
	fs.anyUserReauths = true
	if fs.settings == nil {
		fs.settings = map[string]string{}
	}
	if _, set := fs.settings["apikeys_user_enabled"]; !set {
		fs.settings["apikeys_user_enabled"] = "true"
	}
	return NewAPIKeysHandler(&AppState{
		Store:        fs,
		Authz:        authz.NewResolver(fs),
		FeatureFlags: services.NewFeatureFlags(fs),
	})
}

// sentinelStatus/sentinelBody mark that the wrapped inner handler (the real
// external endpoint) was actually reached, as opposed to the middleware
// answering the request itself.
const (
	sentinelStatus = http.StatusTeapot
	sentinelBody   = "inner-handler-reached"
)

func sentinelInner(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(sentinelStatus)
	w.Write([]byte(sentinelBody))
}

// TestAPIKeyMiddleware_Chain covers every rejection branch of the external
// auth chain (api_keys.go: apiKeyMiddleware) plus the success passthrough.
// Each case gets its own fresh handler/limiters so per-IP and per-key
// rate-limit state never leaks between cases; the dedicated rate-limit tests
// below cover the throttle branches on purpose.
func TestAPIKeyMiddleware_Chain(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	cases := []struct {
		name          string
		authHeader    string
		setAuthHeader bool
		key           *models.APIKey
		requiredPerm  string
		uuidVar       string
		// ownerRoute guards the case with APIKeyOwnerRoute instead of
		// APIKeyServerRoute: no server in the path, SERVER caps must not count.
		ownerRoute bool
		// noUUID suppresses the harness default below, to exercise a
		// server-scoped route wired without the {uuid} it requires.
		noUUID          bool
		wantStatus      int
		wantBodySub     string
		wantInnerCalled bool
	}{
		{
			name:          "missing Authorization header",
			setAuthHeader: false,
			wantStatus:    http.StatusUnauthorized,
			wantBodySub:   "Missing Authorization",
		},
		{
			name:          "Authorization without Bearer prefix",
			setAuthHeader: true,
			authHeader:    "Token abc123",
			wantStatus:    http.StatusUnauthorized,
			wantBodySub:   "Missing Authorization",
		},
		{
			name:          "Bearer with only whitespace trims to an empty key",
			setAuthHeader: true,
			authHeader:    "Bearer    ",
			wantStatus:    http.StatusUnauthorized,
			wantBodySub:   "Invalid key",
		},
		{
			name:          "Bearer dyl_ prefix stripped, hash unknown to the store",
			setAuthHeader: true,
			authHeader:    "Bearer dyl_unknowntoken",
			wantStatus:    http.StatusUnauthorized,
			wantBodySub:   "Invalid key",
		},
		{
			name:            "Bearer without the dyl_ prefix still resolves (prefix strip is optional)",
			setAuthHeader:   true,
			authHeader:      "Bearer noprefixtoken",
			key:             &models.APIKey{ID: 1, RatePerMin: 1000, Scope: models.APIKeyScope{Permissions: []string{"rcon.exec"}}},
			requiredPerm:    "rcon.exec",
			wantInnerCalled: true,
		},
		{
			name:          "revoked key rejected",
			setAuthHeader: true,
			authHeader:    "Bearer revokedtoken",
			key:           &models.APIKey{ID: 2, RatePerMin: 1000, RevokedAt: &past, Scope: models.APIKeyScope{Permissions: []string{"rcon.exec"}}},
			requiredPerm:  "rcon.exec",
			wantStatus:    http.StatusUnauthorized,
			wantBodySub:   "Key revoked",
		},
		{
			name:          "expired key rejected",
			setAuthHeader: true,
			authHeader:    "Bearer expiredtoken",
			key:           &models.APIKey{ID: 3, RatePerMin: 1000, ExpiresAt: &past, Scope: models.APIKeyScope{Permissions: []string{"rcon.exec"}}},
			requiredPerm:  "rcon.exec",
			wantStatus:    http.StatusUnauthorized,
			wantBodySub:   "Key expired",
		},
		{
			name:            "not-yet-expired key (ExpiresAt in the future) is accepted",
			setAuthHeader:   true,
			authHeader:      "Bearer notyetexpired",
			key:             &models.APIKey{ID: 4, RatePerMin: 1000, ExpiresAt: &future, Scope: models.APIKeyScope{Permissions: []string{"rcon.exec"}}},
			requiredPerm:    "rcon.exec",
			wantInnerCalled: true,
		},
		{
			name:          "key lacks the required permission",
			setAuthHeader: true,
			authHeader:    "Bearer nopermtoken",
			key:           &models.APIKey{ID: 5, RatePerMin: 1000, Scope: models.APIKeyScope{Permissions: []string{"other.perm"}}},
			requiredPerm:  "rcon.exec",
			wantStatus:    http.StatusForbidden,
			wantBodySub:   "Key lacks required permission",
		},
		{
			name:          "path server uuid outside the key's scope",
			setAuthHeader: true,
			authHeader:    "Bearer scopedtoken",
			key:           &models.APIKey{ID: 6, RatePerMin: 1000, Scope: models.APIKeyScope{Permissions: []string{"rcon.exec"}, Servers: []string{"other-server-uuid"}}},
			requiredPerm:  "rcon.exec",
			uuidVar:       "target-server-uuid",
			wantStatus:    http.StatusForbidden,
			wantBodySub:   "Key not scoped to this server",
		},
		{
			name:            "path server uuid within the key's scope succeeds",
			setAuthHeader:   true,
			authHeader:      "Bearer inscopetoken",
			key:             &models.APIKey{ID: 7, RatePerMin: 1000, Scope: models.APIKeyScope{Permissions: []string{"rcon.exec"}, Servers: []string{"target-server-uuid"}}},
			requiredPerm:    "rcon.exec",
			uuidVar:         "target-server-uuid",
			wantInnerCalled: true,
		},
		{
			// This case used to assert the opposite - that a missing uuid
			// "skips the AllowsServer check entirely" - and passing was the
			// bug: every SERVER capability on the key counted, for every
			// server, with the allowlist ignored. A server route with no
			// server is a wiring mistake and now says so.
			name:          "server route registered without a uuid is refused, not waved through",
			setAuthHeader: true,
			authHeader:    "Bearer noserverintoken",
			key:           &models.APIKey{ID: 8, RatePerMin: 1000, Scope: models.APIKeyScope{Permissions: []string{"rcon.exec"}, Servers: []string{"target-server-uuid"}}},
			requiredPerm:  "rcon.exec",
			noUUID:        true,
			wantStatus:    http.StatusInternalServerError,
			wantBodySub:   "Route misconfigured",
		},
		{
			// The other half of the same rule: on an owner route there is no
			// server, so a SERVER capability cannot have been scoped to one and
			// must not count. Before the split this key would have been let
			// through on exactly this shape.
			name:          "owner route does not honor a SERVER capability",
			setAuthHeader: true,
			authHeader:    "Bearer ownerroutetoken",
			key:           &models.APIKey{ID: 13, RatePerMin: 1000, Scope: models.APIKeyScope{Permissions: []string{"rcon.exec"}, Servers: []string{"target-server-uuid"}}},
			requiredPerm:  "rcon.exec",
			ownerRoute:    true,
			wantStatus:    http.StatusForbidden,
			wantBodySub:   "Key lacks required permission",
		},
		{
			name:            "non-rcon server cap honored",
			setAuthHeader:   true,
			authHeader:      "Bearer powerstarttoken",
			key:             &models.APIKey{ID: 9, RatePerMin: 1000, Scope: models.APIKeyScope{Permissions: []string{"power.start"}}},
			requiredPerm:    "power.start",
			wantInnerCalled: true,
		},
		{
			name:            "server cap honored within scope",
			setAuthHeader:   true,
			authHeader:      "Bearer filesreadtoken",
			key:             &models.APIKey{ID: 10, RatePerMin: 1000, Scope: models.APIKeyScope{Permissions: []string{"files.read"}, Servers: []string{"target-server-uuid"}}},
			requiredPerm:    "files.read",
			uuidVar:         "target-server-uuid",
			wantInnerCalled: true,
		},
		{
			name:          "panel cap on a key is never honored",
			setAuthHeader: true,
			authHeader:    "Bearer usersreadtoken",
			key:           &models.APIKey{ID: 11, RatePerMin: 1000, Scope: models.APIKeyScope{Permissions: []string{"users.read"}}},
			requiredPerm:  "users.read",
			wantStatus:    http.StatusForbidden,
			wantBodySub:   "Key lacks required permission",
		},
		{
			name:          "key carrying rcon.exec denied a different required cap",
			setAuthHeader: true,
			authHeader:    "Bearer rconexectoken",
			key:           &models.APIKey{ID: 12, RatePerMin: 1000, Scope: models.APIKeyScope{Permissions: []string{"rcon.exec"}}},
			requiredPerm:  "files.read",
			wantStatus:    http.StatusForbidden,
			wantBodySub:   "Key lacks required permission",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The cases here exercise the key's OWN scope and caps. Since the
			// middleware also re-resolves the key owner's current access
			// (ownerStillHolds), every key is owned by the server's owner so that
			// second gate is satisfied and cannot mask what these cases assert.
			// TestAPIKeyMiddleware_OwnerAccessIsRecheckedAtUse covers the gate
			// itself.
			fs := &apiKeysAuthFakeStore{
				keysByHash: map[string]*models.APIKey{},
				users:      map[string]*models.User{"owner-1": {ID: "owner-1", Username: "owner"}},
				servers: map[string]*models.Server{
					"target-server-uuid": {ID: 1, UUID: "target-server-uuid", OwnerID: "owner-1"},
				},
			}
			if c.key != nil {
				c.key.UserID = "owner-1"
				plaintext := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(c.authHeader, "Bearer ")), "dyl_")
				fs.keysByHash[HashAPIKey(plaintext)] = c.key
			}
			h := newAPIKeysAuthHandler(fs)
			wrapped := h.APIKeyServerRoute(c.requiredPerm)(sentinelInner)
			if c.ownerRoute {
				wrapped = h.APIKeyOwnerRoute(c.requiredPerm)(sentinelInner)
			}

			// A server route addresses a server. Cases that do not say which
			// one get the fixture server, in the key's allowlist, so they keep
			// testing the branch they were written for rather than tripping the
			// scope gate. Cases that set Servers themselves are left alone -
			// that is what the scoping cases assert.
			uuidVar := c.uuidVar
			if uuidVar == "" && !c.ownerRoute && !c.noUUID {
				uuidVar = "target-server-uuid"
			}
			if c.key != nil && c.key.Scope.Servers == nil && uuidVar != "" {
				c.key.Scope.Servers = []string{uuidVar}
			}

			r := httptest.NewRequest("POST", "/api/external/rcon", nil)
			if c.setAuthHeader {
				r.Header.Set("Authorization", c.authHeader)
			}
			if uuidVar != "" {
				r = mux.SetURLVars(r, map[string]string{"uuid": uuidVar})
			}
			rec := httptest.NewRecorder()

			wrapped(rec, r)

			if c.wantInnerCalled {
				if rec.Code != sentinelStatus || rec.Body.String() != sentinelBody {
					t.Fatalf("expected inner handler to be reached (status %d, body %q), got status %d body %q",
						sentinelStatus, sentinelBody, rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), c.wantBodySub) {
				t.Fatalf("body = %s, want substring %q", rec.Body.String(), c.wantBodySub)
			}
		})
	}
}

// TestAPIKeyMiddleware_PerKeyRateLimitExceeded pins the per-key sliding
// window: with RatePerMin=1, a second request for the same key inside the
// same minute is throttled with 429 + Retry-After, while the first reaches
// the inner handler.
func TestAPIKeyMiddleware_PerKeyRateLimitExceeded(t *testing.T) {
	// A server route addresses a server, so this fixture carries one and the
	// key is scoped to it; otherwise the request is refused for shape before it
	// ever reaches the limiter this test is about.
	key := &models.APIKey{
		ID: 42, RatePerMin: 1, UserID: "owner-1",
		Scope: models.APIKeyScope{Permissions: []string{"rcon.exec"}, Servers: []string{"target-server-uuid"}},
	}
	fs := &apiKeysAuthFakeStore{
		keysByHash: map[string]*models.APIKey{HashAPIKey("ratelimited"): key},
		users:      map[string]*models.User{"owner-1": {ID: "owner-1", Username: "owner"}},
		servers:    map[string]*models.Server{"target-server-uuid": {ID: 1, UUID: "target-server-uuid", OwnerID: "owner-1"}},
	}
	h := newAPIKeysAuthHandler(fs)
	wrapped := h.APIKeyServerRoute("rcon.exec")(sentinelInner)
	withServer := func(r *http.Request) *http.Request {
		return mux.SetURLVars(r, map[string]string{"uuid": "target-server-uuid"})
	}

	r1 := withServer(httptest.NewRequest("POST", "/api/external/rcon", nil))
	r1.Header.Set("Authorization", "Bearer ratelimited")
	rec1 := httptest.NewRecorder()
	wrapped(rec1, r1)
	if rec1.Code != sentinelStatus {
		t.Fatalf("first request: status = %d, want inner handler reached (%d): %s", rec1.Code, sentinelStatus, rec1.Body.String())
	}

	r2 := withServer(httptest.NewRequest("POST", "/api/external/rcon", nil))
	r2.Header.Set("Authorization", "Bearer ratelimited")
	rec2 := httptest.NewRecorder()
	wrapped(rec2, r2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429: %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q, want 60", rec2.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec2.Body.String(), "Rate limit exceeded") {
		t.Fatalf("body = %s, want 'Rate limit exceeded'", rec2.Body.String())
	}
}

// TestAPIKeyMiddleware_PerIPThrottleExceeded pins the pre-lookup per-IP
// throttle (120/min, hardcoded in api_keys.go): it must trip BEFORE the
// store hash lookup so invalid-key guessing can't hammer the DB unbounded.
// All requests here use an unknown key hash, so every response up to the
// throttle threshold is "Invalid key" (401) and only the 121st request
// becomes "Too many requests" (429).
func TestAPIKeyMiddleware_PerIPThrottleExceeded(t *testing.T) {
	fs := &apiKeysAuthFakeStore{keysByHash: map[string]*models.APIKey{}}
	h := newAPIKeysAuthHandler(fs)
	wrapped := h.APIKeyServerRoute("rcon.exec")(sentinelInner)

	for i := 1; i <= 120; i++ {
		r := httptest.NewRequest("POST", "/api/external/rcon", nil)
		r.Header.Set("Authorization", "Bearer dyl_unknownguess")
		rec := httptest.NewRecorder()
		wrapped(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: status = %d, want 401 (invalid key, IP throttle not yet tripped): %s", i, rec.Code, rec.Body.String())
		}
	}

	r := httptest.NewRequest("POST", "/api/external/rcon", nil)
	r.Header.Set("Authorization", "Bearer dyl_unknownguess")
	rec := httptest.NewRecorder()
	wrapped(rec, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request 121: status = %d, want 429 (IP throttle should trip at 120/min): %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q, want 60", rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "Too many requests") {
		t.Fatalf("body = %s, want 'Too many requests'", rec.Body.String())
	}
}

// --- Create ---

func createAPIKeyReq(userID, username string, isAdmin bool, body map[string]interface{}) *http.Request {
	// Create re-authenticates before minting. A test that is asserting something
	// else should not have to say so; one that IS asserting the gate passes its
	// own password and overrides this.
	if _, set := body["password"]; !set {
		body["password"] = reauthTestPassword
	}
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/api/api-keys", bytes.NewReader(b))
	ctx := context.WithValue(r.Context(), "isAdmin", isAdmin)
	if userID != "" {
		ctx = context.WithValue(ctx, "userID", userID)
	}
	ctx = context.WithValue(ctx, "username", username)
	return r.WithContext(ctx)
}

func TestAPIKeysCreate_Unauthorized(t *testing.T) {
	fs := &apiKeysAuthFakeStore{}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("", "", false, map[string]interface{}{"name": "k", "permissions": []string{"rcon.exec"}}))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeysCreate_InvalidJSON(t *testing.T) {
	fs := &apiKeysAuthFakeStore{}
	h := newAPIKeysAuthHandler(fs)
	r := httptest.NewRequest("POST", "/api/api-keys", bytes.NewReader([]byte("not json")))
	r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "userID", "u1"), "isAdmin", true))
	rec := httptest.NewRecorder()

	h.Create(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeysCreate_NameRequired(t *testing.T) {
	fs := &apiKeysAuthFakeStore{}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("u1", "alice", true, map[string]interface{}{"name": "   ", "permissions": []string{"rcon.exec"}}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Name required") {
		t.Fatalf("body = %s, want 'Name required'", rec.Body.String())
	}
}

func TestAPIKeysCreate_PermissionsRequired(t *testing.T) {
	fs := &apiKeysAuthFakeStore{}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("u1", "alice", true, map[string]interface{}{"name": "k", "permissions": []string{}}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "At least one permission required") {
		t.Fatalf("body = %s, want 'At least one permission required'", rec.Body.String())
	}
}

func TestAPIKeysCreate_UnknownPermissionRejected(t *testing.T) {
	fs := &apiKeysAuthFakeStore{}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("u1", "alice", true, map[string]interface{}{"name": "k", "permissions": []string{"modpack.publish"}}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Unknown permission: modpack.publish") {
		t.Fatalf("body = %s, want 'Unknown permission: modpack.publish'", rec.Body.String())
	}
	if len(fs.createCalls) != 0 {
		t.Fatalf("expected no CreateAPIKey call, got %+v", fs.createCalls)
	}
}

func TestAPIKeysCreate_NonAdminServerNotFound(t *testing.T) {
	fs := &apiKeysAuthFakeStore{servers: map[string]*models.Server{}}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("u1", "alice", false, map[string]interface{}{
		"name": "k", "permissions": []string{"rcon.exec"}, "servers": []string{"missing-uuid"},
	}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Server not found: missing-uuid") {
		t.Fatalf("body = %s, want 'Server not found: missing-uuid'", rec.Body.String())
	}
}

func TestAPIKeysCreate_NonAdminWithoutPowerAccessForbidden(t *testing.T) {
	fs := &apiKeysAuthFakeStore{servers: map[string]*models.Server{
		"srv-1": {ID: 1, UUID: "srv-1", OwnerID: "owner-else"},
	}}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("u1", "alice", false, map[string]interface{}{
		"name": "k", "permissions": []string{"rcon.exec"}, "servers": []string{"srv-1"},
	}))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	// No access at all to the server: refused before the per-capability check.
	if !strings.Contains(rec.Body.String(), "No access to server: srv-1") {
		t.Fatalf("body = %s, want 'No access to server: srv-1'", rec.Body.String())
	}
	if len(fs.createCalls) != 0 {
		t.Fatalf("expected no CreateAPIKey call, got %+v", fs.createCalls)
	}
}

func TestAPIKeysCreate_NonAdminOwnServerSucceeds(t *testing.T) {
	fs := &apiKeysAuthFakeStore{servers: map[string]*models.Server{
		"srv-1": {ID: 1, UUID: "srv-1", OwnerID: "u1"},
	}}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("u1", "alice", false, map[string]interface{}{
		"name": "k", "permissions": []string{"rcon.exec"}, "servers": []string{"srv-1"},
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.createCalls) != 1 {
		t.Fatalf("expected 1 CreateAPIKey call, got %d", len(fs.createCalls))
	}
}

// TestAPIKeysCreate_PanelCapRejected pins that a PANEL-scope capability can
// never be minted onto a key, for BOTH an admin and a non-admin caller: PANEL
// caps belong to staff principals, not owner-scoped automation credentials.
// ValidKeyCap runs before the isAdmin branch, so neither caller can carry one.
func TestAPIKeysCreate_PanelCapRejected(t *testing.T) {
	for _, isAdmin := range []bool{false, true} {
		fs := &apiKeysAuthFakeStore{}
		h := newAPIKeysAuthHandler(fs)
		rec := httptest.NewRecorder()

		h.Create(rec, createAPIKeyReq("u1", "alice", isAdmin, map[string]interface{}{
			"name": "k", "permissions": []string{"users.read"},
		}))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("isAdmin=%v: status = %d, want 400: %s", isAdmin, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Unknown permission: users.read") {
			t.Fatalf("isAdmin=%v: body = %s, want 'Unknown permission: users.read'", isAdmin, rec.Body.String())
		}
		if len(fs.createCalls) != 0 {
			t.Fatalf("isAdmin=%v: expected no CreateAPIKey call, got %+v", isAdmin, fs.createCalls)
		}
	}
}

// TestAPIKeysCreate_ServerCapWithoutServersRejected pins the empty-server-scope
// guard: a non-admin key carrying a SERVER cap but listing no servers is
// rejected at creation, so the per-server subset check is never silently
// skipped (the use-time middleware would also deny such a key, but the creation
// check should not depend on that).
func TestAPIKeysCreate_ServerCapWithoutServersRejected(t *testing.T) {
	fs := &apiKeysAuthFakeStore{}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("u1", "alice", false, map[string]interface{}{
		"name": "k", "permissions": []string{"rcon.exec"},
	}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "require at least one server") {
		t.Fatalf("body = %s, want 'require at least one server'", rec.Body.String())
	}
	if len(fs.createCalls) != 0 {
		t.Fatalf("expected no CreateAPIKey call, got %+v", fs.createCalls)
	}
}

// TestAPIKeysCreate_NonAdminOwnServerBroaderCap proves the subset check
// generalizes beyond rcon.exec: the server owner holds every SERVER cap via
// the resolver's owner short-circuit, so a broader cap like files.delete is
// also allowed on their own server.
func TestAPIKeysCreate_NonAdminOwnServerBroaderCap(t *testing.T) {
	fs := &apiKeysAuthFakeStore{servers: map[string]*models.Server{
		"srv-1": {ID: 1, UUID: "srv-1", OwnerID: "u1"},
	}}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("u1", "alice", false, map[string]interface{}{
		"name": "k", "permissions": []string{"files.delete"}, "servers": []string{"srv-1"},
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.createCalls) != 1 {
		t.Fatalf("expected 1 CreateAPIKey call, got %d", len(fs.createCalls))
	}
}

// TestAPIKeysCreate_FriendPartialGrantSubset is the crown-jewel per-cap
// subset test: a friend holding an explicit grant of ONLY console.read on
// someone else's server may mint a key carrying console.read, but MUST NOT be
// able to also mint files.delete on that key even though it targets the same
// server - the anti-escalation boundary is per capability, not per server.
func TestAPIKeysCreate_FriendPartialGrantSubset(t *testing.T) {
	newFakeStoreWithGrant := func() *apiKeysAuthFakeStore {
		return &apiKeysAuthFakeStore{
			servers: map[string]*models.Server{
				"srv-1": {ID: 1, UUID: "srv-1", OwnerID: "owner-else"},
			},
			grants: map[string]*store.ServerGrant{
				grantKey(1, "u1"): {CapOverrides: store.CapOverrides{Grant: []string{"console.read"}}},
			},
		}
	}

	t.Run("holds console.read: succeeds", func(t *testing.T) {
		fs := newFakeStoreWithGrant()
		h := newAPIKeysAuthHandler(fs)
		rec := httptest.NewRecorder()

		h.Create(rec, createAPIKeyReq("u1", "friend", false, map[string]interface{}{
			"name": "k", "permissions": []string{"console.read"}, "servers": []string{"srv-1"},
		}))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.createCalls) != 1 {
			t.Fatalf("expected 1 CreateAPIKey call, got %d", len(fs.createCalls))
		}
	})

	t.Run("does not hold files.delete: rejected", func(t *testing.T) {
		fs := newFakeStoreWithGrant()
		h := newAPIKeysAuthHandler(fs)
		rec := httptest.NewRecorder()

		h.Create(rec, createAPIKeyReq("u1", "friend", false, map[string]interface{}{
			"name": "k", "permissions": []string{"console.read", "files.delete"}, "servers": []string{"srv-1"},
		}))

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "No files.delete access to server: srv-1") {
			t.Fatalf("body = %s, want 'No files.delete access to server: srv-1'", rec.Body.String())
		}
		if len(fs.createCalls) != 0 {
			t.Fatalf("expected no CreateAPIKey call, got %+v", fs.createCalls)
		}
	})
}

// TestAPIKeysCreate_AdminBypassesScopingAndDefaultsRate pins that an admin
// skips the per-server ownership loop entirely (the server here is not even
// in the fake store, so a non-admin call would 400) and that an
// omitted/non-positive ratePerMin defaults to 60.
func TestAPIKeysCreate_AdminBypassesScopingAndDefaultsRate(t *testing.T) {
	fs := &apiKeysAuthFakeStore{servers: map[string]*models.Server{}}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("admin-1", "root", true, map[string]interface{}{
		"name": "broad-key", "permissions": []string{"rcon.exec"}, "servers": []string{"not-in-store"},
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.createCalls) != 1 {
		t.Fatalf("expected 1 CreateAPIKey call, got %d", len(fs.createCalls))
	}
	call := fs.createCalls[0]
	if call.UserID != "admin-1" || call.Name != "broad-key" || call.RatePerMin != 60 {
		t.Fatalf("call = %+v, want UserID=admin-1 Name=broad-key RatePerMin=60", call)
	}
	if len(call.Scope.Servers) != 1 || call.Scope.Servers[0] != "not-in-store" {
		t.Fatalf("call.Scope.Servers = %+v, want [not-in-store]", call.Scope.Servers)
	}

	var resp createAPIKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success || !strings.HasPrefix(resp.Plaintext, "dyl_") {
		t.Fatalf("response = %+v, want Success=true and a dyl_-prefixed Plaintext", resp)
	}
	// KeyHash carries `json:"-"` (never serialized - the response only ever
	// carries the one-time plaintext), so pin the hash relationship against
	// what was actually persisted via CreateAPIKey, not the JSON response:
	// the stored KeyHash must be exactly HashAPIKey of the plaintext with the
	// dyl_ prefix stripped, since that is what apiKeyMiddleware re-derives
	// from the caller's Authorization header on every subsequent use.
	rawPlaintext := strings.TrimPrefix(resp.Plaintext, "dyl_")
	if call.KeyHash != HashAPIKey(rawPlaintext) {
		t.Fatalf("persisted KeyHash %q does not match HashAPIKey(rawPlaintext) %q", call.KeyHash, HashAPIKey(rawPlaintext))
	}
}

func TestAPIKeysCreate_ExplicitRatePerMinPersisted(t *testing.T) {
	fs := &apiKeysAuthFakeStore{}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("admin-1", "root", true, map[string]interface{}{
		"name": "k", "permissions": []string{"rcon.exec"}, "ratePerMin": 10,
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.createCalls) != 1 || fs.createCalls[0].RatePerMin != 10 {
		t.Fatalf("createCalls = %+v, want one call with RatePerMin=10", fs.createCalls)
	}
}

func TestAPIKeysCreate_StoreErrorIsInternalServerError(t *testing.T) {
	fs := &apiKeysAuthFakeStore{createErr: errors.New("db down")}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()

	h.Create(rec, createAPIKeyReq("admin-1", "root", true, map[string]interface{}{
		"name": "k", "permissions": []string{"rcon.exec"},
	}))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
}

// TestAPIKeyMiddleware_OwnerAccessIsRecheckedAtUse pins that a key carries no
// more authority than its owner holds at the moment it is USED.
//
// Create's doc comment promises "a caller cannot hand a key more access than
// they hold themselves". That was enforced once, at mint time, and then frozen
// into the key's scope row. Removing someone from a server ended their session's
// access and left every key they had minted from it fully working - and revoking
// a member is precisely when the credential they created needs to stop working.
//
// The admin case is the reason the owner is loaded rather than assumed to be an
// ordinary user: Create deliberately skips the delegation check for admins, so
// re-resolving an admin's key as a non-admin would refuse keys that are supposed
// to work.
func TestAPIKeyMiddleware_OwnerAccessIsRecheckedAtUse(t *testing.T) {
	const serverUUID = "target-server-uuid"

	newStore := func(owner *models.User, grant *store.ServerGrant) *apiKeysAuthFakeStore {
		fs := &apiKeysAuthFakeStore{
			keysByHash: map[string]*models.APIKey{},
			users:      map[string]*models.User{},
			servers: map[string]*models.Server{
				// Owned by someone else entirely, so nothing but an explicit
				// grant (or admin) can satisfy the re-check.
				serverUUID: {ID: 1, UUID: serverUUID, OwnerID: "someone-else"},
			},
			grants: map[string]*store.ServerGrant{},
		}
		if owner != nil {
			fs.users[owner.ID] = owner
		}
		if grant != nil && owner != nil {
			fs.grants[grantKey(1, owner.ID)] = grant
		}
		fs.keysByHash[HashAPIKey("thekey")] = &models.APIKey{
			ID: 1, UserID: "friend-1", RatePerMin: 1000,
			Scope: models.APIKeyScope{Permissions: []string{"rcon.exec"}, Servers: []string{serverUUID}},
		}
		return fs
	}

	grantWith := func(caps ...string) *store.ServerGrant {
		return &store.ServerGrant{CapOverrides: store.CapOverrides{Grant: caps}}
	}

	friend := &models.User{ID: "friend-1", Username: "friend"}
	adminOwner := &models.User{ID: "friend-1", Username: "root", IsAdmin: true}

	tests := []struct {
		name        string
		fs          *apiKeysAuthFakeStore
		wantStatus  int
		wantInner   bool
		wantBodySub string
	}{
		{
			name:      "grant still in place: the key works",
			fs:        newStore(friend, grantWith("rcon.exec")),
			wantInner: true,
		},
		{
			name:        "grant revoked since the key was minted",
			fs:          newStore(friend, nil),
			wantStatus:  http.StatusForbidden,
			wantBodySub: "no longer has this access",
		},
		{
			name:        "grant narrowed to a different capability",
			fs:          newStore(friend, grantWith("console.read")),
			wantStatus:  http.StatusForbidden,
			wantBodySub: "no longer has this access",
		},
		{
			name:      "admin key stays unrestricted, as Create intends",
			fs:        newStore(adminOwner, nil),
			wantInner: true,
		},
		{
			name:        "owner account gone",
			fs:          newStore(nil, nil),
			wantStatus:  http.StatusUnauthorized,
			wantBodySub: "no longer valid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newAPIKeysAuthHandler(tt.fs)
			wrapped := h.APIKeyServerRoute("rcon.exec")(sentinelInner)

			r := httptest.NewRequest("POST", "/api/external/rcon", nil)
			r.Header.Set("Authorization", "Bearer dyl_thekey")
			r = mux.SetURLVars(r, map[string]string{"uuid": serverUUID})
			rec := httptest.NewRecorder()

			wrapped(rec, r)

			if tt.wantInner {
				if rec.Code != sentinelStatus {
					t.Fatalf("expected the inner handler to run, got %d: %s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantBodySub) {
				t.Errorf("body = %q, want it to mention %q", rec.Body.String(), tt.wantBodySub)
			}
		})
	}
}

// The gate on Create, asserted where it lives.
//
// An API key authenticates by its OWN hash, so it keeps working after the
// password change that kills every session - which makes minting one the way a
// borrowed session turns into a permanent one. The other Create tests send the
// password through createAPIKeyReq's default because they are asserting the
// delegation maths; this one is what fails if requireReauth is ever taken back
// out of Create.
func TestAPIKeysCreate_RefusesTheWrongAccountPassword(t *testing.T) {
	for name, password := range map[string]string{
		"wrong":   "not-the-password",
		"missing": "",
	} {
		t.Run(name+" password", func(t *testing.T) {
			fs := &apiKeysAuthFakeStore{servers: map[string]*models.Server{
				"srv-1": {ID: 1, UUID: "srv-1", OwnerID: "u1"},
			}}
			h := newAPIKeysAuthHandler(fs)
			rec := httptest.NewRecorder()

			h.Create(rec, createAPIKeyReq("u1", "alice", false, map[string]interface{}{
				"name": "k", "permissions": []string{"rcon.exec"},
				"servers": []string{"srv-1"}, "password": password,
			}))

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401: %s", rec.Code, rec.Body.String())
			}
			if len(fs.createCalls) != 0 {
				t.Errorf("a key was minted without the account password: %+v", fs.createCalls)
			}
		})
	}
}
