package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
)

func (f *apiKeysAuthFakeStore) GetBackupJob(id int) (*models.BackupJob, error) {
	j, ok := f.backupJobs[id]
	if !ok {
		return nil, errors.New("job not found")
	}
	return j, nil
}

func (f *apiKeysAuthFakeStore) ListServersForUser(userID string, isAdmin bool) ([]models.Server, error) {
	if isAdmin {
		return f.fleetServers, nil
	}
	return f.ownedServers, nil
}

// externalFixture is the shape every test here shares: one key, one owner, and
// the servers the store knows about.
func externalFixture(perms []string, scopedServers []string, owner *models.User, servers map[string]*models.Server) *apiKeysAuthFakeStore {
	return &apiKeysAuthFakeStore{
		keysByHash: map[string]*models.APIKey{
			HashAPIKey("thekey"): {
				ID: 1, RatePerMin: 1000, UserID: owner.ID,
				Scope: models.APIKeyScope{Permissions: perms, Servers: scopedServers},
			},
		},
		users:   map[string]*models.User{owner.ID: owner},
		servers: servers,
	}
}

func externalRequest(method, path string, vars map[string]string, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer thekey")
	return mux.SetURLVars(r, vars)
}

// The bypass this guard exists for. The panel handlers behind the backup-job
// routes resolve their server from the JOB ROW, not from the path, and they
// authorize against the key OWNER - who holds backups.* on all of their own
// servers. So the {uuid} that the allowlist check reads and the server the
// handler acts on are two different values, and only the first one is scoped.
func TestExternalJobInServer_RefusesAJobFromASiblingServer(t *testing.T) {
	owner := &models.User{ID: "owner-1", Username: "owner"}
	fs := externalFixture([]string{"backups.create"}, []string{"uuid-a"}, owner, map[string]*models.Server{
		"uuid-a": {ID: 10, UUID: "uuid-a", OwnerID: "owner-1"},
		"uuid-b": {ID: 20, UUID: "uuid-b", OwnerID: "owner-1"},
	})
	fs.backupJobs = map[int]*models.BackupJob{
		7: {ID: 7, ServerID: 20}, // belongs to the server the key is NOT scoped to
		8: {ID: 8, ServerID: 10},
	}
	h := newAPIKeysAuthHandler(fs)
	wrapped := h.APIKeyServerRoute("backups.create")(h.ExternalJobInServer(sentinelInner))

	t.Run("a job on another server is refused even though the path names an allowed one", func(t *testing.T) {
		rec := httptest.NewRecorder()
		wrapped(rec, externalRequest("POST", "/api/external/servers/uuid-a/backup-jobs/7/trigger",
			map[string]string{"uuid": "uuid-a", "jobId": "7"}, ""))

		if rec.Code == sentinelStatus {
			t.Fatal("the inner handler was reached: a key scoped to uuid-a triggered a job belonging to uuid-b")
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a job on the named server passes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		wrapped(rec, externalRequest("POST", "/api/external/servers/uuid-a/backup-jobs/8/trigger",
			map[string]string{"uuid": "uuid-a", "jobId": "8"}, ""))

		if rec.Code != sentinelStatus {
			t.Fatalf("status = %d, want the inner handler reached: %s", rec.Code, rec.Body.String())
		}
	})
}

// The power route carries no route capability (the action is in the body), so
// the middleware cannot check one and ServerPowerHandler resolves it against
// the OWNER. For an ADMIN owner that resolves to everything, which is why the
// key's own capability has to be checked separately.
func TestAPIKeyPowerGate_ChecksTheKeyNotTheOwner(t *testing.T) {
	adminOwner := &models.User{ID: "owner-1", Username: "root", IsAdmin: true}
	servers := map[string]*models.Server{"uuid-a": {ID: 10, UUID: "uuid-a", OwnerID: "owner-1"}}

	run := func(t *testing.T, perms []string, action string) *httptest.ResponseRecorder {
		t.Helper()
		h := newAPIKeysAuthHandler(externalFixture(perms, []string{"uuid-a"}, adminOwner, servers))
		wrapped := h.APIKeyServerRoute("")(h.APIKeyPowerGate(sentinelInner))
		rec := httptest.NewRecorder()
		wrapped(rec, externalRequest("POST", "/api/external/servers/uuid-a/power",
			map[string]string{"uuid": "uuid-a"}, `{"action":"`+action+`"}`))
		return rec
	}

	t.Run("an action the key was not minted for is refused", func(t *testing.T) {
		rec := run(t, []string{"console.send"}, "start")
		if rec.Code == sentinelStatus {
			t.Fatal("the inner handler was reached: a key minted for console.send started a server because its owner is an admin")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a key holding only power.stop cannot start", func(t *testing.T) {
		if rec := run(t, []string{"power.stop"}, "start"); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the action the key does hold passes", func(t *testing.T) {
		if rec := run(t, []string{"power.stop"}, "stop"); rec.Code != sentinelStatus {
			t.Fatalf("status = %d, want the inner handler reached: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unknown action resolves to no capability and is refused", func(t *testing.T) {
		if rec := run(t, []string{"power.start", "power.stop"}, "nuke"); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	// The gate reads the body to learn the action; the handler behind it decodes
	// the same body again. If the bytes are not put back it decodes an empty
	// stream and every power request fails as malformed.
	t.Run("the body survives the peek", func(t *testing.T) {
		h := newAPIKeysAuthHandler(externalFixture([]string{"power.start"}, []string{"uuid-a"}, adminOwner, servers))
		var seen string
		inner := func(w http.ResponseWriter, r *http.Request) {
			b := make([]byte, 64)
			n, _ := r.Body.Read(b)
			seen = string(b[:n])
			w.WriteHeader(sentinelStatus)
		}
		wrapped := h.APIKeyServerRoute("")(h.APIKeyPowerGate(inner))
		wrapped(httptest.NewRecorder(), externalRequest("POST", "/api/external/servers/uuid-a/power",
			map[string]string{"uuid": "uuid-a"}, `{"action":"start"}`))

		if seen != `{"action":"start"}` {
			t.Errorf("handler read %q, want the original body: the peek consumed it", seen)
		}
	})
}

// A listing route has no {uuid}, so the allowlist cannot gate it in the
// middleware - the handler has to apply it. Scoping by owner alone would return
// every server the owner has, which is wider than the key was minted for.
func TestListExternalServers_FiltersToTheKeyAllowlist(t *testing.T) {
	owner := &models.User{ID: "owner-1", Username: "owner"}
	fs := externalFixture(nil, []string{"uuid-a"}, owner, nil)
	fs.ownedServers = []models.Server{
		{ID: 10, UUID: "uuid-a", Name: "in-scope", OwnerID: "owner-1"},
		{ID: 20, UUID: "uuid-b", Name: "sibling", OwnerID: "owner-1"},
		{ID: 30, UUID: "uuid-c", Name: "another", OwnerID: "owner-1"},
	}
	h := newAPIKeysAuthHandler(fs)
	wrapped := h.APIKeyOwnerRoute("")(h.ListExternalServers)

	rec := httptest.NewRecorder()
	wrapped(rec, externalRequest("GET", "/api/external/servers", nil, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "uuid-a") {
		t.Errorf("body = %s, want the in-scope server", body)
	}
	for _, out := range []string{"uuid-b", "uuid-c"} {
		if strings.Contains(body, out) {
			t.Errorf("body = %s, want %s excluded: the key is not scoped to it", body, out)
		}
	}
}

// An operator's support key, scoped to a customer's server. The per-server
// routes answer it - the middleware re-resolves the key owner, who is an admin,
// and admins hold everything. The listing has to agree, or it reports the key as
// having nothing to act on while every other route in the surface works.
//
// The allowlist, not the owner query, is what bounds this: fleetServers holds
// three servers the owner does not own and only the allowlisted one comes back.
func TestListExternalServers_AnAdminsKeyListsWhatItCanActOn(t *testing.T) {
	owner := &models.User{ID: "owner-1", Username: "root", IsAdmin: true}
	fs := externalFixture(nil, []string{"uuid-a"}, owner, nil)
	fs.ownedServers = nil // the admin owns none of them
	fs.fleetServers = []models.Server{
		{ID: 10, UUID: "uuid-a", Name: "customer-server"},
		{ID: 20, UUID: "uuid-b", Name: "another-tenant"},
		{ID: 30, UUID: "uuid-c", Name: "third-tenant"},
	}
	h := newAPIKeysAuthHandler(fs)
	wrapped := h.APIKeyOwnerRoute("")(h.ListExternalServers)

	rec := httptest.NewRecorder()
	wrapped(rec, externalRequest("GET", "/api/external/servers", nil, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "uuid-a") {
		t.Errorf("body = %s, want the allowlisted server: the key answers 200 for it on every other route", body)
	}
	for _, out := range []string{"uuid-b", "uuid-c"} {
		if strings.Contains(body, out) {
			t.Errorf("body = %s, want %s excluded: an admin's key is still bounded by its allowlist", body, out)
		}
	}
}

func TestListExternalServers_EmptyAllowlistListsNothing(t *testing.T) {
	owner := &models.User{ID: "owner-1", Username: "owner"}
	fs := externalFixture(nil, nil, owner, nil)
	fs.ownedServers = []models.Server{{ID: 10, UUID: "uuid-a", Name: "owned"}}
	h := newAPIKeysAuthHandler(fs)
	wrapped := h.APIKeyOwnerRoute("")(h.ListExternalServers)

	rec := httptest.NewRecorder()
	wrapped(rec, externalRequest("GET", "/api/external/servers", nil, ""))

	if strings.Contains(rec.Body.String(), "uuid-a") {
		t.Errorf("body = %s, want no servers: a key with an empty allowlist can address none", rec.Body.String())
	}
}

// The adapter is what lets a key route reuse a panel handler unchanged. Both
// halves have to be right: the {id} the handler reads, and the identity it
// resolves authority from.
func TestExternalServerRoute_HandsThePanelHandlerIDAndOwner(t *testing.T) {
	owner := &models.User{ID: "owner-1", Username: "owner"}
	fs := externalFixture([]string{"console.read"}, []string{"uuid-a"}, owner,
		map[string]*models.Server{"uuid-a": {ID: 42, UUID: "uuid-a", OwnerID: "owner-1"}})
	h := newAPIKeysAuthHandler(fs)

	var gotID, gotUser, gotName string
	var gotAdmin bool
	inner := func(w http.ResponseWriter, r *http.Request) {
		gotID = mux.Vars(r)["id"]
		gotUser, _ = r.Context().Value("userID").(string)
		gotName, _ = r.Context().Value("username").(string)
		gotAdmin, _ = r.Context().Value("isAdmin").(bool)
		w.WriteHeader(sentinelStatus)
	}
	wrapped := h.APIKeyServerRoute("console.read")(h.ExternalServerRoute(inner))

	rec := httptest.NewRecorder()
	wrapped(rec, externalRequest("GET", "/api/external/servers/uuid-a/console/history",
		map[string]string{"uuid": "uuid-a"}, ""))

	if rec.Code != sentinelStatus {
		t.Fatalf("status = %d, want the inner handler reached: %s", rec.Code, rec.Body.String())
	}
	if gotID != "42" {
		t.Errorf("mux id = %q, want 42: a panel handler reads {id}, not {uuid}", gotID)
	}
	if gotUser != "owner-1" || gotName != "owner" || gotAdmin {
		t.Errorf("identity = (%q, %q, admin=%v), want the key owner: a handler that resolves authority from the context needs a real principal", gotUser, gotName, gotAdmin)
	}
}

// Registering an adapter without the key middleware in front of it would run a
// panel handler with no principal at all. That must be loud, not a request that
// quietly resolves as the empty user.
func TestExternalAdaptersRefuseARequestWithNoKey(t *testing.T) {
	h := newAPIKeysAuthHandler(&apiKeysAuthFakeStore{})
	for name, wrapped := range map[string]http.HandlerFunc{
		"server adapter": h.ExternalServerRoute(sentinelInner),
		"owner adapter":  h.ExternalOwnerRoute(sentinelInner),
		"power gate":     h.APIKeyPowerGate(sentinelInner),
	} {
		t.Run(name, func(t *testing.T) {
			r := mux.SetURLVars(httptest.NewRequest("GET", "/api/external/servers/uuid-a", nil),
				map[string]string{"uuid": "uuid-a"})
			rec := httptest.NewRecorder()
			wrapped(rec, r)
			if rec.Code == sentinelStatus {
				t.Fatal("the inner handler was reached with no key in the context")
			}
		})
	}
}
