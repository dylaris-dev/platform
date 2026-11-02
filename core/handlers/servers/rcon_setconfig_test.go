package servers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// rconConfigFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods SetConfig (and the
// checkServerAccess helper it calls) touch are overridden. Any other call
// would panic - these tests never make one.
type rconConfigFakeStore struct {
	store.Store

	server    *models.Server
	serverErr error

	invite    *models.ServerInvite
	inviteErr error

	rconEnabled  bool
	rconPort     int
	rconPassword string
	getRconErr   error

	logFilter bool

	setRconCalls []rconSetCall
	setRconErr   error

	applyPropsCalls []applyPropsCall
	applyPropsErr   error

	needsRestart         bool
	setNeedsRestartCalls []bool
	setNeedsRestartErr   error
}

type rconSetCall struct {
	serverID int
	enabled  bool
	port     int
	password string
}

type applyPropsCall struct {
	nodeID          int
	serverUUID      string
	activeSubServer string
	enabled         bool
	port            int
	password        string
}

func (f *rconConfigFakeStore) GetServerByID(id int) (*models.Server, error) {
	return f.server, f.serverErr
}

func (f *rconConfigFakeStore) GetInvite(serverID int, userID string) (*models.ServerInvite, error) {
	return f.invite, f.inviteErr
}

func (f *rconConfigFakeStore) GetServerRconConfig(serverID int) (bool, int, string, error) {
	return f.rconEnabled, f.rconPort, f.rconPassword, f.getRconErr
}

func (f *rconConfigFakeStore) GetServerRconLogFilter(serverID int) (bool, error) {
	return f.logFilter, nil
}

func (f *rconConfigFakeStore) SetServerRconLogFilter(serverID int, on bool) error {
	f.logFilter = on
	return nil
}

func (f *rconConfigFakeStore) SetServerRconConfig(serverID int, enabled bool, port int, password string) error {
	f.setRconCalls = append(f.setRconCalls, rconSetCall{serverID, enabled, port, password})
	return f.setRconErr
}

func (f *rconConfigFakeStore) GetServerRconNeedsRestart(serverID int) (bool, error) {
	return f.needsRestart, nil
}

func (f *rconConfigFakeStore) SetServerRconNeedsRestart(serverID int, needsRestart bool) error {
	f.setNeedsRestartCalls = append(f.setNeedsRestartCalls, needsRestart)
	return f.setNeedsRestartErr
}

func newRconConfigHandler(fs *rconConfigFakeStore) *RconHandler {
	h := &RconHandler{state: withCoreStorage(&handlers.AppState{Store: fs})}
	// Inject a fake props-writer so these tests exercise SetConfig's own logic
	// without a live node/gRPC registry. It records the call and returns the
	// configured error (nil by default = success).
	h.applyProps = func(nodeID int, serverUUID, activeSubServer string, enabled bool, port int, password string) error {
		fs.applyPropsCalls = append(fs.applyPropsCalls, applyPropsCall{nodeID, serverUUID, activeSubServer, enabled, port, password})
		return fs.applyPropsErr
	}
	return h
}

func rconSetConfigReq(serverID int, username string, isAdmin bool, userID string, body map[string]interface{}) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("PUT", "/api/servers/"+strconv.Itoa(serverID)+"/rcon/config", bytes.NewReader(b))
	r = mux.SetURLVars(r, map[string]string{"id": strconv.Itoa(serverID)})
	ctx := context.WithValue(r.Context(), "username", username)
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	ctx = context.WithValue(ctx, "userID", userID)
	return r.WithContext(ctx)
}

func decodeRconConfigResp(t *testing.T, rec *httptest.ResponseRecorder) rconConfigResponse {
	t.Helper()
	var out rconConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

// --- Access control ---
//
// Phase 4 Task 4: config.read/config.write are now enforced at the route
// chokepoint (RequireCap wrapping in routes.go), not in-handler, so the old
// non-owner/no-invite and invited-without-power forbidden cases moved to
// routes_authz_test.go (TestCap_RconConfigReadVsWrite) where they run through
// the real resolver. What remains here is the handler's own, still-live
// "server not found" existence check (independent of authz).

func TestRconSetConfig_ServerNotFound(t *testing.T) {
	fs := &rconConfigFakeStore{serverErr: errors.New("no rows")}
	h := newRconConfigHandler(fs)
	rec := httptest.NewRecorder()
	h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": true, "port": 25575}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestRconSetConfig_InvalidJSON(t *testing.T) {
	fs := &rconConfigFakeStore{server: &models.Server{ID: 1, OwnerName: "alice"}}
	h := newRconConfigHandler(fs)
	r := httptest.NewRequest("PUT", "/api/servers/1/rcon/config", bytes.NewReader([]byte("not json")))
	r = mux.SetURLVars(r, map[string]string{"id": "1"})
	ctx := context.WithValue(r.Context(), "username", "alice")
	ctx = context.WithValue(ctx, "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", "u1")
	r = r.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.SetConfig(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// --- Port-range guard ---

func TestRconSetConfig_PortRangeGuard(t *testing.T) {
	cases := []struct {
		name string
		port int
		want int
	}{
		{"negative port rejected", -1, http.StatusBadRequest},
		{"port over 65535 rejected", 65536, http.StatusBadRequest},
		{"port 0 (use existing) accepted", 0, http.StatusOK},
		{"port 25575 accepted", 25575, http.StatusOK},
		{"port 65535 (max valid) accepted", 65535, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &rconConfigFakeStore{server: &models.Server{ID: 1, OwnerName: "alice"}}
			h := newRconConfigHandler(fs)
			rec := httptest.NewRecorder()
			h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": false, "port": c.port}))
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			if c.want == http.StatusBadRequest && len(fs.setRconCalls) != 0 {
				t.Fatalf("expected no persist on rejected port, got %+v", fs.setRconCalls)
			}
		})
	}
}

// --- Password regen/keep branching (rcon.go:241-254) ---

func TestRconSetConfig_PasswordBranching(t *testing.T) {
	t.Run("regenerate=true always mints a new password even with one already stored", func(t *testing.T) {
		fs := &rconConfigFakeStore{
			server:       &models.Server{ID: 1, OwnerName: "alice", ActiveSubServer: "server"},
			rconEnabled:  true,
			rconPassword: "old-password",
		}
		h := newRconConfigHandler(fs)
		rec := httptest.NewRecorder()
		h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": true, "port": 25575, "regenerate": true}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.setRconCalls) != 1 {
			t.Fatalf("setRconCalls = %+v, want 1 call", fs.setRconCalls)
		}
		persisted := fs.setRconCalls[0].password
		if persisted == "" || persisted == "old-password" {
			t.Fatalf("persisted password = %q, want a freshly generated value different from the old one", persisted)
		}
		resp := decodeRconConfigResp(t, rec)
		if resp.Password != persisted {
			t.Fatalf("response.Password = %q, want the newly minted password %q to be exposed once", resp.Password, persisted)
		}
	})

	t.Run("enabled=true with no stored password auto-regenerates", func(t *testing.T) {
		fs := &rconConfigFakeStore{
			server:       &models.Server{ID: 1, OwnerName: "alice", ActiveSubServer: "server"},
			rconEnabled:  false,
			rconPassword: "",
		}
		h := newRconConfigHandler(fs)
		rec := httptest.NewRecorder()
		h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": true, "port": 25575}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.setRconCalls) != 1 || fs.setRconCalls[0].password == "" {
			t.Fatalf("setRconCalls = %+v, want 1 call with a non-empty generated password", fs.setRconCalls)
		}
		resp := decodeRconConfigResp(t, rec)
		if resp.Password == "" || !resp.HasSecret {
			t.Fatalf("response = %+v, want a newly minted password exposed once", resp)
		}
	})

	t.Run("enabled=true with an existing stored password and regenerate=false keeps it", func(t *testing.T) {
		fs := &rconConfigFakeStore{
			server:       &models.Server{ID: 1, OwnerName: "alice", ActiveSubServer: "server"},
			rconEnabled:  true,
			rconPassword: "existing-secret",
		}
		h := newRconConfigHandler(fs)
		rec := httptest.NewRecorder()
		h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": true, "port": 25575}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.setRconCalls) != 1 || fs.setRconCalls[0].password != "existing-secret" {
			t.Fatalf("setRconCalls = %+v, want the existing password kept unchanged", fs.setRconCalls)
		}
		resp := decodeRconConfigResp(t, rec)
		if resp.Password != "" {
			t.Fatalf("response.Password = %q, want empty (never re-exposing a kept password)", resp.Password)
		}
		if !resp.HasSecret {
			t.Fatalf("response.HasSecret = false, want true (a password is set)")
		}
	})

	t.Run("an explicit request password is used verbatim when not regenerating and enabled requires none", func(t *testing.T) {
		fs := &rconConfigFakeStore{
			server:       &models.Server{ID: 1, OwnerName: "alice"},
			rconEnabled:  false,
			rconPassword: "",
		}
		h := newRconConfigHandler(fs)
		rec := httptest.NewRecorder()
		// enabled=false here, so the "Enabled && password==''" auto-regen branch
		// does not fire; the explicit req.Password is used instead.
		h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": false, "port": 25575, "password": "caller-supplied"}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.setRconCalls) != 1 || fs.setRconCalls[0].password != "caller-supplied" {
			t.Fatalf("setRconCalls = %+v, want the caller-supplied password persisted verbatim", fs.setRconCalls)
		}
	})

	t.Run("disabling with no password and no regenerate leaves password empty", func(t *testing.T) {
		fs := &rconConfigFakeStore{server: &models.Server{ID: 1, OwnerName: "alice"}}
		h := newRconConfigHandler(fs)
		rec := httptest.NewRecorder()
		h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": false, "port": 25575}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.setRconCalls) != 1 || fs.setRconCalls[0].password != "" {
			t.Fatalf("setRconCalls = %+v, want an empty persisted password", fs.setRconCalls)
		}
		resp := decodeRconConfigResp(t, rec)
		if resp.HasSecret {
			t.Fatalf("response.HasSecret = true, want false (no password set)")
		}
	})

	t.Run("port 0 falls back to the existing stored port", func(t *testing.T) {
		fs := &rconConfigFakeStore{
			server:      &models.Server{ID: 1, OwnerName: "alice", ActiveSubServer: "server"},
			rconEnabled: true,
			rconPort:    25580,
		}
		h := newRconConfigHandler(fs)
		rec := httptest.NewRecorder()
		h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": true, "port": 0}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.setRconCalls) != 1 || fs.setRconCalls[0].port != 25580 {
			t.Fatalf("setRconCalls = %+v, want port 25580 (the existing stored value)", fs.setRconCalls)
		}
	})
}

func TestRconSetConfig_PersistFailure(t *testing.T) {
	fs := &rconConfigFakeStore{server: &models.Server{ID: 1, OwnerName: "alice"}, setRconErr: errors.New("db down")}
	h := newRconConfigHandler(fs)
	rec := httptest.NewRecorder()
	h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": false, "port": 25575}))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
}

// --- server.properties propagation (the connection-refused fix) ---

// Enabling on an installed server writes enable-rcon into server.properties and
// only then persists the DB flag, using the same resolved port for both.
func TestRconSetConfig_WritesServerProperties(t *testing.T) {
	fs := &rconConfigFakeStore{
		server:       &models.Server{ID: 1, OwnerName: "alice", UUID: "srv-uuid", NodeID: 7, ActiveSubServer: "survival"},
		rconEnabled:  false,
		rconPassword: "kept-secret",
	}
	h := newRconConfigHandler(fs)
	rec := httptest.NewRecorder()
	h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": true, "port": 25575}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.applyPropsCalls) != 1 {
		t.Fatalf("applyPropsCalls = %+v, want exactly 1", fs.applyPropsCalls)
	}
	call := fs.applyPropsCalls[0]
	if call.nodeID != 7 || call.serverUUID != "srv-uuid" || call.activeSubServer != "survival" {
		t.Fatalf("applyProps target = %+v, want node 7 / srv-uuid / survival", call)
	}
	if !call.enabled || call.port != 25575 || call.password != "kept-secret" {
		t.Fatalf("applyProps args = %+v, want enabled/port 25575/kept-secret", call)
	}
	// DB persisted with the same values.
	if len(fs.setRconCalls) != 1 || !fs.setRconCalls[0].enabled || fs.setRconCalls[0].port != 25575 {
		t.Fatalf("setRconCalls = %+v, want the same enabled/port persisted", fs.setRconCalls)
	}
}

// A failure writing server.properties aborts the whole request: the DB flag is
// NOT flipped, so the DB and the file never diverge.
func TestRconSetConfig_PropsFailureAborts(t *testing.T) {
	fs := &rconConfigFakeStore{
		server:        &models.Server{ID: 1, OwnerName: "alice", UUID: "srv-uuid", NodeID: 7, ActiveSubServer: "survival"},
		rconPassword:  "kept-secret",
		applyPropsErr: errors.New("node offline"),
	}
	h := newRconConfigHandler(fs)
	rec := httptest.NewRecorder()
	h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": true, "port": 25575}))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if len(fs.setRconCalls) != 0 {
		t.Fatalf("setRconCalls = %+v, want NO DB write after a props-write failure", fs.setRconCalls)
	}
}

// Enabling on a server that has not been installed yet (no active sub-server)
// is rejected before touching server.properties or the DB.
func TestRconSetConfig_EnableRequiresActiveSubServer(t *testing.T) {
	fs := &rconConfigFakeStore{server: &models.Server{ID: 1, OwnerName: "alice", ActiveSubServer: ""}}
	h := newRconConfigHandler(fs)
	rec := httptest.NewRecorder()
	h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": true, "port": 25575}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if len(fs.applyPropsCalls) != 0 || len(fs.setRconCalls) != 0 {
		t.Fatalf("expected no props-write and no DB write; props=%+v db=%+v", fs.applyPropsCalls, fs.setRconCalls)
	}
}

// Disabling a not-yet-installed server records the DB flag without a props-write
// (there is no file to touch and nothing is listening).
func TestRconSetConfig_DisableSkipsPropsWhenNoSubServer(t *testing.T) {
	fs := &rconConfigFakeStore{server: &models.Server{ID: 1, OwnerName: "alice", ActiveSubServer: ""}}
	h := newRconConfigHandler(fs)
	rec := httptest.NewRecorder()
	h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": false, "port": 25575}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.applyPropsCalls) != 0 {
		t.Fatalf("applyPropsCalls = %+v, want none (nothing installed to write)", fs.applyPropsCalls)
	}
	if len(fs.setRconCalls) != 1 || fs.setRconCalls[0].enabled {
		t.Fatalf("setRconCalls = %+v, want one call with enabled=false", fs.setRconCalls)
	}
}

// --- restartRequired signal (the "raw connection-refused" fix) ---
//
// MC only opens/re-reads the RCON listener at JVM start, so a server.properties
// write against a server that is already running is inert until it restarts.
// SetConfig must NEVER trigger that restart itself (owner decision: no
// auto-restart) - it only has to report whether one is needed, so the panel
// can render a deterministic "restart required" state instead of inferring it
// from a later connection-refused error. restartRequired must track exactly
// whether server.properties was actually touched (srv.ActiveSubServer != ""),
// independent of the enabled/disabled direction, and the DB flag/properties
// must still be persisted exactly as before - with no restart triggered from
// this handler.
func TestRconSetConfig_RestartRequired(t *testing.T) {
	cases := []struct {
		name            string
		activeSubServer string
		enabled         bool
		wantRestart     bool
	}{
		{"enabling on an installed server needs a restart", "survival", true, true},
		{"disabling on an installed server still rewrites the file, so it needs a restart too", "survival", false, true},
		{"disabling on a not-yet-installed server touches nothing, no restart needed", "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &rconConfigFakeStore{
				server:       &models.Server{ID: 1, OwnerName: "alice", UUID: "srv-uuid", NodeID: 7, ActiveSubServer: c.activeSubServer},
				rconPassword: "kept-secret",
			}
			h := newRconConfigHandler(fs)
			rec := httptest.NewRecorder()
			h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": c.enabled, "port": 25575}))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			resp := decodeRconConfigResp(t, rec)
			if resp.RestartRequired != c.wantRestart {
				t.Fatalf("RestartRequired = %v, want %v", resp.RestartRequired, c.wantRestart)
			}
			// The flag is now also PERSISTED (so it survives a panel reload),
			// matching the value reported in the response.
			if len(fs.setNeedsRestartCalls) != 1 || fs.setNeedsRestartCalls[0] != c.wantRestart {
				t.Fatalf("setNeedsRestartCalls = %+v, want one call persisting %v", fs.setNeedsRestartCalls, c.wantRestart)
			}
			// The DB flag is still persisted exactly as before - this endpoint
			// reports the need for a restart, it does not perform one.
			if len(fs.setRconCalls) != 1 || fs.setRconCalls[0].enabled != c.enabled {
				t.Fatalf("setRconCalls = %+v, want one call with enabled=%v", fs.setRconCalls, c.enabled)
			}
			wantPropsCalls := 0
			if c.activeSubServer != "" {
				wantPropsCalls = 1
			}
			if len(fs.applyPropsCalls) != wantPropsCalls {
				t.Fatalf("applyPropsCalls = %+v, want %d call(s)", fs.applyPropsCalls, wantPropsCalls)
			}
		})
	}
}

// GetConfig surfaces the PERSISTED needs-restart flag as RestartRequired, so a
// panel reload restores the banner and keeps the RCON-dependent Players tabs
// locked instead of silently unlocking them before the restart happens.
func TestRconGetConfig_SurfacesPersistedNeedsRestart(t *testing.T) {
	for _, persisted := range []bool{true, false} {
		fs := &rconConfigFakeStore{
			server:       &models.Server{ID: 1, OwnerName: "alice", ActiveSubServer: "survival"},
			rconEnabled:  true,
			rconPort:     25575,
			rconPassword: "secret",
			needsRestart: persisted,
		}
		h := newRconConfigHandler(fs)
		r := httptest.NewRequest("GET", "/api/servers/1/rcon/config", nil)
		r = mux.SetURLVars(r, map[string]string{"id": "1"})
		rec := httptest.NewRecorder()
		h.GetConfig(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		resp := decodeRconConfigResp(t, rec)
		if resp.RestartRequired != persisted {
			t.Fatalf("RestartRequired = %v, want %v (the persisted flag)", resp.RestartRequired, persisted)
		}
	}
}

// A failure persisting the needs-restart flag fails the whole SetConfig the
// same way a config-persist failure does, so the DB never ends up with the
// rcon_enabled and needs_restart columns out of sync.
func TestRconSetConfig_NeedsRestartPersistFailure(t *testing.T) {
	fs := &rconConfigFakeStore{
		server:             &models.Server{ID: 1, OwnerName: "alice", UUID: "srv-uuid", NodeID: 7, ActiveSubServer: "survival"},
		rconPassword:       "kept-secret",
		setNeedsRestartErr: errors.New("db down"),
	}
	h := newRconConfigHandler(fs)
	rec := httptest.NewRecorder()
	h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": true, "port": 25575}))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
}

// hideLogNoise is a POINTER in the request. A plain bool would read as false on
// every client that does not send the field, so saving an unrelated RCON change
// (port, password) would silently turn the console filter back off.
func TestRconSetConfig_OmittedLogFilterKeepsStoredValue(t *testing.T) {
	fs := &rconConfigFakeStore{server: &models.Server{ID: 1, OwnerName: "alice", ActiveSubServer: "main"}, logFilter: true}
	h := newRconConfigHandler(fs)

	rec := httptest.NewRecorder()
	h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{"enabled": true, "port": 25575}))

	if !fs.logFilter {
		t.Error("an unrelated RCON save turned the console filter off")
	}
	if resp := decodeRconConfigResp(t, rec); !resp.HideLogNoise {
		t.Errorf("response hideLogNoise = false, want the stored true")
	}
}

func TestRconSetConfig_LogFilterTogglesBothWays(t *testing.T) {
	for _, want := range []bool{true, false} {
		fs := &rconConfigFakeStore{server: &models.Server{ID: 1, OwnerName: "alice", ActiveSubServer: "main"}, logFilter: !want}
		h := newRconConfigHandler(fs)

		rec := httptest.NewRecorder()
		h.SetConfig(rec, rconSetConfigReq(1, "alice", false, "u1", map[string]interface{}{
			"enabled": true, "port": 25575, "hideLogNoise": want,
		}))

		if fs.logFilter != want {
			t.Errorf("stored filter = %v, want %v", fs.logFilter, want)
		}
		if resp := decodeRconConfigResp(t, rec); resp.HideLogNoise != want {
			t.Errorf("response hideLogNoise = %v, want %v", resp.HideLogNoise, want)
		}
	}
}
