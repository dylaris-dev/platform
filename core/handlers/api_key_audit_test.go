package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/handlers/audit"
	"dylaris-core/models"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// Audit side of apiKeysAuthFakeStore. A server with no entry in auditOn has
// auditing OFF, which is the default for a server nobody was invited to.

func (f *apiKeysAuthFakeStore) GetServerAuditState(id int) (bool, bool, int, error) {
	return f.auditOn[id], false, 0, nil
}
func (f *apiKeysAuthFakeStore) SetServerAuditEnabled(id int, on bool) error {
	if f.auditOn == nil {
		f.auditOn = map[int]bool{}
	}
	f.auditOn[id] = on
	return nil
}
func (f *apiKeysAuthFakeStore) InsertServerAudit(e *models.ServerAuditEvent) error {
	e.ID = int64(len(f.serverAudits) + 1)
	f.serverAudits = append(f.serverAudits, *e)
	return nil
}
func (f *apiKeysAuthFakeStore) SetServerAuditCount(id, n int64) error {
	f.serverAudits[id-1].Metadata["count"] = n
	return nil
}
func (f *apiKeysAuthFakeStore) InsertAuditIdentity(e *models.AuditEventIdentity) error {
	e.ID = int64(len(f.identityAudits) + 1)
	f.identityAudits = append(f.identityAudits, *e)
	return nil
}
func (f *apiKeysAuthFakeStore) SetAuditIdentityCount(id, n int64) error {
	f.identityAudits[id-1].Metadata["count"] = n
	return nil
}
func (f *apiKeysAuthFakeStore) CancelUserDeletion(id string) error {
	f.cancelledFor = append(f.cancelledFor, id)
	return nil
}

func keyAuditFixture(perms []string) (*apiKeysAuthFakeStore, *APIKeysHandler) {
	owner := &models.User{ID: "owner-1", Username: "bartis"}
	fs := externalFixture(perms, []string{"uuid-a"}, owner, map[string]*models.Server{
		"uuid-a": {ID: 10, UUID: "uuid-a", OwnerID: "owner-1"},
		"uuid-b": {ID: 20, UUID: "uuid-b", OwnerID: "someone-else"},
	})
	fs.keysByHash[HashAPIKey("thekey")].Name = "deploy-bot"
	return fs, newAPIKeysAuthHandler(fs)
}

func serve(h http.HandlerFunc, method, uuid string) int {
	rec := httptest.NewRecorder()
	h(rec, externalRequest(method, "/api/external/servers/"+uuid+"/x", map[string]string{"uuid": uuid}, `{}`))
	return rec.Code
}

// G-1, G-2: a key's write on a server with auditing off left no record, and
// what was recorded named only the owner.
func TestKeyWriteIsRecordedWithTheKeyAndSwitchesAuditingOn(t *testing.T) {
	fs, h := keyAuditFixture([]string{"console.send"})
	if code := serve(h.APIKeyServerRoute("console.send")(reachedInner), "POST", "uuid-a"); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !fs.auditOn[10] {
		t.Fatal("auditing still off on the server the key acted on")
	}
	if len(fs.serverAudits) != 1 {
		t.Fatalf("rows %+v, want one", fs.serverAudits)
	}
	ev := fs.serverAudits[0]
	if ev.EventType != "console.send" || ev.ActorUserID == nil || *ev.ActorUserID != "owner-1" ||
		ev.ActorAPIKeyID == nil || *ev.ActorAPIKeyID != 1 || ev.ActorAPIKeyName != "deploy-bot" {
		t.Fatalf("row %+v, want console.send by owner-1 with key 1 deploy-bot", ev)
	}
}

// A handler that writes its own entry (power_action) gets the key stamped on
// it, and the middleware does not add a second one.
func TestKeyActionAHandlerDescribesIsRecordedOnce(t *testing.T) {
	fs, h := keyAuditFixture([]string{"power.start"})
	inner := func(w http.ResponseWriter, r *http.Request) {
		audit.LogServer(h.state.Store, r, 10, audit.ServerAuditEventPowerAction, "owner-1", "", map[string]interface{}{"action": "start"})
		w.WriteHeader(http.StatusOK)
	}
	if code := serve(h.APIKeyServerRoute("")(inner), "POST", "uuid-a"); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(fs.serverAudits) != 1 || fs.serverAudits[0].EventType != audit.ServerAuditEventPowerAction ||
		fs.serverAudits[0].ActorAPIKeyName != "deploy-bot" {
		t.Fatalf("rows %+v, want exactly one power_action carrying the key", fs.serverAudits)
	}
}

func TestPanelActionOnAServerWithAuditingOffStaysUnrecorded(t *testing.T) {
	fs, h := keyAuditFixture(nil)
	audit.LogServer(h.state.Store, httptest.NewRequest("POST", "/x", nil), 10, "console.send", "owner-1", "", nil)
	if len(fs.serverAudits) != 0 || fs.auditOn[10] {
		t.Fatalf("rows %+v auditOn %v; a request without a key must not switch auditing on", fs.serverAudits, fs.auditOn[10])
	}
}

func withRedis(t *testing.T, h *APIKeysHandler) *miniredis.Miniredis {
	mr := miniredis.RunT(t)
	h.state.Redis = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return mr
}

func TestRefusedKeyRequestsAreRecordedBundled(t *testing.T) {
	t.Run("in scope: on the server's trail, one entry with a count", func(t *testing.T) {
		fs, h := keyAuditFixture([]string{"console.read"})
		withRedis(t, h)
		for i := 0; i < 2; i++ {
			if code := serve(h.APIKeyServerRoute("console.send")(reachedInner), "POST", "uuid-a"); code != http.StatusForbidden {
				t.Fatalf("status %d, want 403", code)
			}
		}
		if len(fs.serverAudits) != 1 || fs.serverAudits[0].EventType != audit.ServerAuditEventAPIKeyDenied ||
			fs.serverAudits[0].ActorAPIKeyName != "deploy-bot" || fs.serverAudits[0].Metadata["count"] != int64(2) {
			t.Fatalf("rows %+v, want one api_key_denied with count 2", fs.serverAudits)
		}
	})
	t.Run("out of scope: identity log, the other server untouched", func(t *testing.T) {
		fs, h := keyAuditFixture([]string{"console.send"})
		if code := serve(h.APIKeyServerRoute("console.send")(reachedInner), "POST", "uuid-b"); code != http.StatusForbidden {
			t.Fatalf("status %d, want 403", code)
		}
		if len(fs.serverAudits) != 0 || fs.auditOn[20] {
			t.Fatalf("someone else's server got rows %+v / auditing %v", fs.serverAudits, fs.auditOn[20])
		}
		if len(fs.identityAudits) != 1 || fs.identityAudits[0].EventType != audit.AuditEventAPIKeyDenied {
			t.Fatalf("identity rows %+v, want one api_key_denied", fs.identityAudits)
		}
	})
	t.Run("in scope but the owner lost the server: identity log", func(t *testing.T) {
		fs, h := keyAuditFixture([]string{"console.send"})
		fs.keysByHash[HashAPIKey("thekey")].Scope.Servers = []string{"uuid-b"} // owner holds nothing there
		if code := serve(h.APIKeyServerRoute("console.send")(reachedInner), "POST", "uuid-b"); code != http.StatusForbidden {
			t.Fatalf("status %d, want 403", code)
		}
		if len(fs.serverAudits) != 0 || fs.auditOn[20] {
			t.Fatalf("a server the owner cannot reach got rows %+v / auditing %v", fs.serverAudits, fs.auditOn[20])
		}
		if len(fs.identityAudits) != 1 || fs.identityAudits[0].EventType != audit.AuditEventAPIKeyDenied {
			t.Fatalf("identity rows %+v, want one api_key_denied", fs.identityAudits)
		}
	})
	t.Run("revoked key", func(t *testing.T) {
		fs, h := keyAuditFixture([]string{"console.send"})
		now := time.Now()
		fs.keysByHash[HashAPIKey("thekey")].RevokedAt = &now
		if code := serve(h.APIKeyServerRoute("console.send")(reachedInner), "POST", "uuid-a"); code != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", code)
		}
		if len(fs.serverAudits) != 1 || fs.serverAudits[0].EventType != audit.ServerAuditEventAPIKeyDenied {
			t.Fatalf("rows %+v, want one api_key_denied", fs.serverAudits)
		}
	})
}

// A real key refused again and again is throttled like a guess.
func TestRefusalsOfAValidKeyAreThrottledPerIP(t *testing.T) {
	_, h := keyAuditFixture([]string{"console.read"})
	route := h.APIKeyServerRoute("console.send")(reachedInner)
	for i := 0; i < keyFailuresPerMin; i++ {
		if code := serve(route, "POST", "uuid-a"); code != http.StatusForbidden {
			t.Fatalf("refusal %d: status %d, want 403", i+1, code)
		}
	}
	if code := serve(route, "POST", "uuid-a"); code != http.StatusTooManyRequests {
		t.Fatalf("refusal past the limit: status %d, want 429", code)
	}
}

// Owner decision: reads are one entry per key, server and kind per 10 minutes,
// with a count. The count is written back sparsely: at powers of two and at
// most every keyCountEvery otherwise.
func TestKeyReadsAreBundledWithASparseCount(t *testing.T) {
	fs, h := keyAuditFixture([]string{"console.read", "stats.read"})
	mr := withRedis(t, h)
	read := func() { serve(h.APIKeyServerRoute("console.read")(reachedInner), "GET", "uuid-a") }
	for i := 0; i < 5; i++ {
		read()
	}
	serve(h.APIKeyServerRoute("stats.read")(reachedInner), "GET", "uuid-a")
	if len(fs.serverAudits) != 2 {
		t.Fatalf("rows %+v, want one per kind", fs.serverAudits)
	}
	if ev := fs.serverAudits[0]; ev.EventType != "console.read" || ev.Metadata["count"] != int64(4) {
		t.Fatalf("console.read row %+v, want count 4 (5th not written back yet)", ev)
	}
	mr.FastForward(keyCountEvery + time.Second)
	read()
	if c := fs.serverAudits[0].Metadata["count"]; c != int64(6) {
		t.Fatalf("count %v after the interval, want 6", c)
	}
	if ttl := mr.TTL("audit:apikey:1:10:console.read"); ttl <= 0 {
		t.Fatalf("counter TTL %v, want one", ttl)
	}
	mr.FastForward(keyAuditWindow)
	read()
	if len(fs.serverAudits) != 3 {
		t.Fatalf("a read after the window did not start a new entry: %d rows", len(fs.serverAudits))
	}
	if ttl := mr.TTL("audit:apikey:1:10:console.read"); ttl <= 0 || ttl > keyAuditWindow {
		t.Fatalf("recreated counter TTL %v, want 0 < ttl <= window", ttl)
	}
}

// G-3: the operator's list was not checked at use for power, the one route
// whose capability is in the body.
func TestPowerCapabilityTakenOffTheOperatorListStopsWorking(t *testing.T) {
	fs, h := keyAuditFixture([]string{"power.start"})
	fs.settings["apikeys_user_allowed_caps"] = "console.read"
	route := h.APIKeyServerRoute("")(h.APIKeyPowerGate(reachedInner))
	rec := httptest.NewRecorder()
	route(rec, externalRequest("POST", "/api/external/servers/uuid-a/power", map[string]string{"uuid": "uuid-a"}, `{"action":"start"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
}

// G-4: an anonymized account kept working keys; a pending one is in use.
func TestKeyOwnerDeletionState(t *testing.T) {
	fs, h := keyAuditFixture([]string{"console.send"})
	fs.users["owner-1"].DeletionStatus = "anonymized"
	if code := serve(h.APIKeyServerRoute("console.send")(reachedInner), "POST", "uuid-a"); code != http.StatusUnauthorized {
		t.Fatalf("anonymized owner: status %d, want 401", code)
	}
	fs.users["owner-1"].DeletionStatus = "pending_deletion"
	if code := serve(h.APIKeyServerRoute("console.send")(reachedInner), "POST", "uuid-a"); code != http.StatusOK {
		t.Fatalf("pending owner: status %d, want 200", code)
	}
	if len(fs.cancelledFor) != 1 {
		t.Fatalf("deletion not cancelled by key use: %v", fs.cancelledFor)
	}
	// A key the operator gate refuses is not in use.
	fs.cancelledFor = nil
	fs.settings["apikeys_user_enabled"] = "false"
	h = newAPIKeysAuthHandler(fs) // feature flags are cached per handler
	if code := serve(h.APIKeyServerRoute("console.send")(reachedInner), "POST", "uuid-a"); code != http.StatusForbidden {
		t.Fatalf("keys off: status %d, want 403", code)
	}
	if len(fs.cancelledFor) != 0 {
		t.Fatalf("a refused key cancelled the deletion: %v", fs.cancelledFor)
	}
}

// G-8: valid requests are no longer charged to the per-IP limit, failures are.
func TestIPLimitCountsOnlyFailedKeys(t *testing.T) {
	fs, h := keyAuditFixture([]string{"console.read"})
	fs.keysByHash[HashAPIKey("thekey")].RatePerMin = 600
	route := h.APIKeyServerRoute("console.read")(reachedInner)
	for i := 0; i < keyFailuresPerMin+10; i++ {
		if code := serve(route, "GET", "uuid-a"); code != http.StatusOK {
			t.Fatalf("valid request %d: status %d", i+1, code)
		}
	}
	bad := func() int {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/x", nil)
		r.Header.Set("Authorization", "Bearer nope")
		route(rec, mux.SetURLVars(r, map[string]string{"uuid": "uuid-a"}))
		return rec.Code
	}
	for i := 0; i < keyFailuresPerMin; i++ {
		if code := bad(); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401", i+1, code)
		}
	}
	if code := bad(); code != http.StatusTooManyRequests {
		t.Fatalf("failure past the limit: status %d, want 429", code)
	}
}

func TestCreateValidatesTheKey(t *testing.T) {
	owner := &models.User{ID: "owner-1", Username: "bartis"}
	cases := []struct {
		name string
		body map[string]interface{}
		live []models.APIKey
		want int
	}{
		{"too long", map[string]interface{}{"name": strings.Repeat("a", 65)}, nil, http.StatusBadRequest},
		{"newline", map[string]interface{}{"name": "a\nSystem"}, nil, http.StatusBadRequest},
		{"fake owner", map[string]interface{}{"name": "bot (owner admin)"}, nil, http.StatusBadRequest},
		{"duplicate", map[string]interface{}{"name": "Deploy"}, []models.APIKey{{ID: 5, Name: "deploy"}}, http.StatusConflict},
		{"revoked duplicate is fine", map[string]interface{}{"name": "deploy"}, []models.APIKey{{ID: 5, Name: "deploy", RevokedAt: &time.Time{}}}, http.StatusOK},
		{"expiry too long", map[string]interface{}{"name": "k", "expiresInDays": 366}, nil, http.StatusBadRequest},
		{"key-managing capability", map[string]interface{}{"name": "k", "permissions": []string{"apikeys.write"}}, nil, http.StatusBadRequest},
		{"ok with expiry", map[string]interface{}{"name": strings.Repeat("a", 64), "expiresInDays": 30}, nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := externalFixture(nil, nil, owner, map[string]*models.Server{"uuid-a": {ID: 10, UUID: "uuid-a", OwnerID: "owner-1"}})
			fs.listedKeys = tc.live
			fs.users = nil // any caller re-authenticates (newAPIKeysAuthHandler)
			h := newAPIKeysAuthHandler(fs)
			if _, ok := tc.body["permissions"]; !ok {
				tc.body["permissions"] = []string{"console.send"}
				tc.body["servers"] = []string{"uuid-a"}
			}
			rec := httptest.NewRecorder()
			h.Create(rec, createAPIKeyReq("owner-1", "bartis", false, tc.body))
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK {
				return
			}
			k := fs.createCalls[0]
			if days, _ := tc.body["expiresInDays"].(int); days > 0 {
				if k.ExpiresAt == nil || time.Until(*k.ExpiresAt) < 29*24*time.Hour {
					t.Fatalf("expiresAt %v, want ~30 days out", k.ExpiresAt)
				}
			} else if k.ExpiresAt != nil {
				t.Fatalf("expiresAt %v, want never", k.ExpiresAt)
			}
			if !fs.auditOn[10] {
				t.Fatal("auditing not switched on for the server in the key's scope")
			}
			if len(fs.identityAudits) != 1 || fs.identityAudits[0].EventType != audit.AuditEventAPIKeyCreated {
				t.Fatalf("identity rows %+v, want api_key_created", fs.identityAudits)
			}
			var resp createAPIKeyResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if !resp.Success {
				t.Fatalf("response %s", rec.Body.String())
			}
		})
	}
}
