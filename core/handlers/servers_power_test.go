package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// serverPowerFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods ServerPowerHandler and
// byonNodeServerCapReached touch are overridden. Any other call would panic -
// these tests never make one.
type serverPowerFakeStore struct {
	store.Store

	server    *models.Server
	serverErr error

	invite    *models.ServerInvite
	inviteErr error

	billing    *store.UserBilling
	billingErr error

	node    *models.Node
	nodeErr error

	settings map[string]string

	countServersByNode    int
	countServersByNodeErr error

	// serverGrant + serverRole back the authz.Resolver's delegation path (a
	// non-owner holding a per-server capability grant): set both to give
	// serverGrant.UserID the caps listed on serverRole.Capabilities.
	serverGrant *store.ServerGrant
	serverRole  *store.ServerRole

	updateDesiredStateCalls []serverPowerDesiredStateCall
	updateStatusCalls       []serverPowerStatusCall

	auditEnabled   bool
	insertedAudits []*models.ServerAuditEvent

	setRconNeedsRestartCalls []bool
}

// The following satisfy authz.Store (beyond GetServerByID, already overridden
// above) so h.state.Authz.Resolve does not panic on the nil embedded
// store.Store. None of these tests exercise panel roles or account grants, so
// they report "not found" unless a test explicitly sets serverGrant/serverRole.
func (f *serverPowerFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	return nil, errors.New("not found")
}

func (f *serverPowerFakeStore) GetPanelRole(id int) (*store.PanelRole, error) {
	return nil, errors.New("not found")
}

func (f *serverPowerFakeStore) GetServerRole(id int) (*store.ServerRole, error) {
	if f.serverRole != nil && f.serverRole.ID == id {
		return f.serverRole, nil
	}
	return nil, errors.New("not found")
}

func (f *serverPowerFakeStore) GetUserPanelAuthz(userID string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func (f *serverPowerFakeStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	if f.serverGrant != nil && f.serverGrant.UserID == userID {
		return f.serverGrant, nil
	}
	return nil, errors.New("not found")
}

func (f *serverPowerFakeStore) GetAccountGrant(ownerUserID, userID string) (*store.ServerGrant, error) {
	return nil, errors.New("not found")
}

type serverPowerDesiredStateCall struct {
	id    int
	state string
}

type serverPowerStatusCall struct {
	id     int
	status string
}

func (f *serverPowerFakeStore) GetServerByID(id int) (*models.Server, error) {
	return f.server, f.serverErr
}

func (f *serverPowerFakeStore) GetInvite(serverID int, userID string) (*models.ServerInvite, error) {
	return f.invite, f.inviteErr
}

func (f *serverPowerFakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	if f.billingErr != nil {
		return nil, f.billingErr
	}
	if f.billing != nil {
		return f.billing, nil
	}
	return &store.UserBilling{UserID: userID, Status: "active"}, nil
}

func (f *serverPowerFakeStore) GetNodeByID(id int) (*models.Node, error) {
	return f.node, f.nodeErr
}

func (f *serverPowerFakeStore) UpdateServerDesiredState(id int, desiredState string) error {
	f.updateDesiredStateCalls = append(f.updateDesiredStateCalls, serverPowerDesiredStateCall{id, desiredState})
	return nil
}

func (f *serverPowerFakeStore) UpdateServerStatus(id int, status string) error {
	f.updateStatusCalls = append(f.updateStatusCalls, serverPowerStatusCall{id, status})
	return nil
}

func (f *serverPowerFakeStore) SetServerRconNeedsRestart(serverID int, needsRestart bool) error {
	f.setRconNeedsRestartCalls = append(f.setRconNeedsRestartCalls, needsRestart)
	return nil
}

// GetServerAuditState reports audit disabled by default (LogServerAudit then
// no-ops); set auditEnabled to exercise the audit-insert path.
func (f *serverPowerFakeStore) GetServerAuditState(serverID int) (bool, bool, int, error) {
	return f.auditEnabled, false, 0, nil
}

func (f *serverPowerFakeStore) InsertServerAudit(ev *models.ServerAuditEvent) error {
	f.insertedAudits = append(f.insertedAudits, ev)
	return nil
}

func (f *serverPowerFakeStore) GetSetting(key string) (string, error) {
	return f.settings[key], nil
}

func (f *serverPowerFakeStore) CountServersByNode(nodeID int) (int, error) {
	return f.countServersByNode, f.countServersByNodeErr
}

// newServerPowerRedis returns a real *redis.Client backed by miniredis, used
// both for the install-cooldown TTL key and (wrapped in a QueueService) for
// the node command stream.
func newServerPowerRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

func newServerPowerHandler(fs *serverPowerFakeStore, rdb *redis.Client) *ServerHandler {
	return &ServerHandler{state: &AppState{
		Store: fs,
		Redis: rdb,
		Queue: services.NewQueueService(rdb),
		Authz: authz.NewResolver(fs),
	}}
}

// serverPowerReq builds the exact request ServerPowerHandler expects: JSON
// body {"action": ...}, mux var "id", and the username/isAdmin/userID
// identity keys AuthMiddleware sets on the context (auth.go:194-197).
func serverPowerReq(serverID int, action, username string, isAdmin bool, userID string) *http.Request {
	body, _ := json.Marshal(map[string]string{"action": action})
	r := httptest.NewRequest("POST", "/api/servers/"+strconv.Itoa(serverID)+"/power", bytes.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"id": strconv.Itoa(serverID)})
	ctx := context.WithValue(r.Context(), "username", username)
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	ctx = context.WithValue(ctx, "userID", userID)
	return r.WithContext(ctx)
}

func decodeErrBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error body: %v (body=%s)", err, rec.Body.String())
	}
	return out.Message
}

// --- Action validation ---

func TestServerPowerHandler_InvalidAction(t *testing.T) {
	cases := []string{"", "bogus", "START", "reboot"}
	for _, action := range cases {
		t.Run("action="+action, func(t *testing.T) {
			fs := &serverPowerFakeStore{}
			h := newServerPowerHandler(fs, newServerPowerRedis(t))
			rec := httptest.NewRecorder()
			h.ServerPowerHandler(rec, serverPowerReq(1, action, "alice", false, "u1"))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if msg := decodeErrBody(t, rec); msg != "Invalid action" {
				t.Fatalf("message = %q, want %q", msg, "Invalid action")
			}
		})
	}
}

func TestServerPowerHandler_InvalidJSON(t *testing.T) {
	fs := &serverPowerFakeStore{}
	h := newServerPowerHandler(fs, newServerPowerRedis(t))
	r := httptest.NewRequest("POST", "/api/servers/1/power", bytes.NewReader([]byte("not json")))
	r = mux.SetURLVars(r, map[string]string{"id": "1"})
	ctx := context.WithValue(r.Context(), "username", "alice")
	ctx = context.WithValue(ctx, "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", "u1")
	r = r.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServerPowerHandler(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestServerPowerHandler_ServerNotFound(t *testing.T) {
	fs := &serverPowerFakeStore{serverErr: errors.New("no rows")}
	h := newServerPowerHandler(fs, newServerPowerRedis(t))
	rec := httptest.NewRecorder()
	h.ServerPowerHandler(rec, serverPowerReq(99, "start", "alice", false, "u1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// --- Blocked states ---

func TestServerPowerHandler_PendingSetupBlocked(t *testing.T) {
	// OwnerID, so the caller is entitled to act and the state check is what
	// answers. Without it this passed for the wrong reason: the state checks
	// used to run before authorization, so a stranger got this message too.
	fs := &serverPowerFakeStore{server: &models.Server{ID: 1, Status: "pending_setup", OwnerName: "alice", OwnerID: "u1"}}
	h := newServerPowerHandler(fs, newServerPowerRedis(t))
	rec := httptest.NewRecorder()
	h.ServerPowerHandler(rec, serverPowerReq(1, "start", "alice", false, "u1"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeErrBody(t, rec); msg != "Server is not set up yet" {
		t.Fatalf("message = %q", msg)
	}
}

// A caller with no right to the server learns nothing about its state.
//
// The authorization used to run AFTER the state checks, so the same stranger
// got "Server is not set up yet" (400) for a server still installing, a 403 for
// a running one and a 404 for an id that does not exist - enough to enumerate
// ids and their rough state from any account.
func TestServerPowerHandler_StateSaysNothingToAStranger(t *testing.T) {
	for _, status := range []string{"pending_setup", "disk_full", "online", "stopped"} {
		t.Run(status, func(t *testing.T) {
			fs := &serverPowerFakeStore{server: &models.Server{ID: 1, Status: status, OwnerName: "alice", OwnerID: "someone-else"}}
			h := newServerPowerHandler(fs, newServerPowerRedis(t))
			rec := httptest.NewRecorder()
			h.ServerPowerHandler(rec, serverPowerReq(1, "start", "mallory", false, "u-mallory"))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if msg := decodeErrBody(t, rec); msg != "Forbidden" {
				t.Fatalf("message = %q, want the same refusal whatever the state", msg)
			}
		})
	}
}

// TestServerPowerHandler_DiskFull pins the disk_full precedence
// (servers_lifecycle.go:704-708 runs BEFORE the isOffline/isOnline 409 matrix):
// start/restart get the dedicated disk-full message, while stop/kill fall
// through to the generic "not running" 409 because disk_full also counts as
// isOffline.
func TestServerPowerHandler_DiskFull(t *testing.T) {
	cases := []struct {
		action     string
		wantStatus int
		wantMsg    string
	}{
		{"start", http.StatusBadRequest, "Server cannot start - storage limit reached. Delete files or raise the limit."},
		{"restart", http.StatusBadRequest, "Server cannot start - storage limit reached. Delete files or raise the limit."},
		{"stop", http.StatusConflict, "Server is not running"},
		{"kill", http.StatusConflict, "Server is not running"},
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			fs := &serverPowerFakeStore{server: &models.Server{ID: 1, Status: "disk_full", OwnerName: "alice", OwnerID: "u1"}}
			h := newServerPowerHandler(fs, newServerPowerRedis(t))
			rec := httptest.NewRecorder()
			h.ServerPowerHandler(rec, serverPowerReq(1, c.action, "alice", false, "u1"))
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if msg := decodeErrBody(t, rec); msg != c.wantMsg {
				t.Fatalf("message = %q, want %q", msg, c.wantMsg)
			}
		})
	}
}

// --- Access control (owner / admin / invited member) ---

func TestServerPowerHandler_AccessControl(t *testing.T) {
	t.Run("non-owner without invite is forbidden", func(t *testing.T) {
		fs := &serverPowerFakeStore{
			server:    &models.Server{ID: 1, Status: "stopped", OwnerName: "alice"},
			inviteErr: errors.New("no invite"),
		}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "mallory", false, "u2"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invited member without power permission is forbidden", func(t *testing.T) {
		fs := &serverPowerFakeStore{
			server: &models.Server{ID: 1, Status: "stopped", OwnerName: "alice"},
			invite: &models.ServerInvite{Permissions: models.TabPermissions{Power: false}},
		}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "bob", false, "u3"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	// The legacy per-invite "power" flag is superseded by the authz.Resolver's
	// ServerGrant/ServerRole delegation path (Phase 4 cut-over): a non-owner
	// now needs an actual capability grant, not an Invite.Permissions.Power flag.
	t.Run("server-grant holder with power.start proceeds", func(t *testing.T) {
		roleID := 9
		fs := &serverPowerFakeStore{
			server:      &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerName: "alice", OwnerID: "u1", NodeID: 5},
			node:        &models.Node{ID: 5, Token: "node-tok"},
			serverGrant: &store.ServerGrant{UserID: "u3", ServerRoleID: &roleID},
			serverRole:  &store.ServerRole{ID: roleID, Capabilities: []string{"power.start"}},
		}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "bob", false, "u3"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	// Per-action distinction: a power.start-only grant must not also unlock kill.
	t.Run("server-grant holder with power.start only is forbidden on kill", func(t *testing.T) {
		roleID := 9
		fs := &serverPowerFakeStore{
			server:      &models.Server{ID: 1, UUID: "srv-uuid", Status: "online", OwnerName: "alice", OwnerID: "u1", NodeID: 5},
			node:        &models.Node{ID: 5, Token: "node-tok"},
			serverGrant: &store.ServerGrant{UserID: "u3", ServerRoleID: &roleID},
			serverRole:  &store.ServerRole{ID: roleID, Capabilities: []string{"power.start"}},
		}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "kill", "bob", false, "u3"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("owner (non-admin) passes without any grant", func(t *testing.T) {
		fs := &serverPowerFakeStore{
			server: &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerName: "alice", OwnerID: "u1", NodeID: 5},
			node:   &models.Node{ID: 5, Token: "node-tok"},
		}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "alice", false, "u1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})
}

// A (re)start clears the persisted rcon_needs_restart flag (the server re-reads
// server.properties at boot, so a pending RCON change is now live), while
// stop/kill leave it untouched. This is what keeps the panel's RCON banner +
// Players-tab lock honest across a reload.
func TestServerPowerHandler_ClearsRconNeedsRestartOnStart(t *testing.T) {
	cases := []struct {
		action    string
		status    string
		wantClear bool
	}{
		{"start", "stopped", true},
		{"restart", "online", true},
		{"stop", "online", false},
		{"kill", "online", false},
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			fs := &serverPowerFakeStore{
				server: &models.Server{ID: 1, UUID: "srv-uuid", Status: c.status, OwnerName: "alice", OwnerID: "u1", NodeID: 5},
				node:   &models.Node{ID: 5, Token: "node-tok"},
			}
			h := newServerPowerHandler(fs, newServerPowerRedis(t))
			rec := httptest.NewRecorder()
			h.ServerPowerHandler(rec, serverPowerReq(1, c.action, "alice", false, "u1"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if c.wantClear {
				if len(fs.setRconNeedsRestartCalls) != 1 || fs.setRconNeedsRestartCalls[0] {
					t.Fatalf("setRconNeedsRestartCalls = %+v, want exactly one false-clear", fs.setRconNeedsRestartCalls)
				}
			} else if len(fs.setRconNeedsRestartCalls) != 0 {
				t.Fatalf("setRconNeedsRestartCalls = %+v, want none (stop/kill must not clear)", fs.setRconNeedsRestartCalls)
			}
		})
	}
}

// --- Server-level suspension (distinct from billing suspension) ---

func TestServerPowerHandler_ServerSuspended(t *testing.T) {
	t.Run("non-admin blocked", func(t *testing.T) {
		fs := &serverPowerFakeStore{server: &models.Server{ID: 1, Status: "suspended", OwnerName: "alice", OwnerID: "u1"}}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "alice", false, "u1"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if msg := decodeErrBody(t, rec); msg != "Server is suspended. Action blocked." {
			t.Fatalf("message = %q", msg)
		}
	})

	// "suspended" is neither isOffline nor isOnline (servers_lifecycle.go:757-758),
	// so once an admin bypasses the suspended-status gate, the status-transition
	// switch does not block it either - the action proceeds all the way through.
	t.Run("admin bypasses and the transition guard does not re-block it", func(t *testing.T) {
		fs := &serverPowerFakeStore{
			server: &models.Server{ID: 1, UUID: "srv-uuid", Status: "suspended", OwnerName: "alice", NodeID: 5},
			node:   &models.Node{ID: 5, Token: "node-tok"},
		}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "root", true, "admin1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})
}

// --- BYON billing suspend (tenant-level, independent of server.Status) ---

func TestServerPowerHandler_BillingSuspended(t *testing.T) {
	t.Run("start blocked for non-admin when billing suspended", func(t *testing.T) {
		fs := &serverPowerFakeStore{
			server:  &models.Server{ID: 1, Status: "stopped", OwnerName: "alice", OwnerID: "u1"},
			billing: &store.UserBilling{UserID: "u1", Status: "suspended"},
		}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "alice", false, "u1"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if msg := decodeErrBody(t, rec); msg != suspendedMessage {
			t.Fatalf("message = %q", msg)
		}
	})

	t.Run("restart blocked for non-admin when billing suspended", func(t *testing.T) {
		fs := &serverPowerFakeStore{
			server:  &models.Server{ID: 1, Status: "online", OwnerName: "alice", OwnerID: "u1"},
			billing: &store.UserBilling{UserID: "u1", Status: "suspended"},
		}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "restart", "alice", false, "u1"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("stop is NOT gated by billing suspension", func(t *testing.T) {
		fs := &serverPowerFakeStore{
			server:  &models.Server{ID: 1, UUID: "srv-uuid", Status: "online", OwnerName: "alice", OwnerID: "u1", NodeID: 5},
			billing: &store.UserBilling{UserID: "u1", Status: "suspended"},
			node:    &models.Node{ID: 5, Token: "node-tok"},
		}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "stop", "alice", false, "u1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("admin bypasses the billing-suspend gate", func(t *testing.T) {
		fs := &serverPowerFakeStore{
			server:  &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerName: "alice", OwnerID: "owner-1", NodeID: 5},
			billing: &store.UserBilling{UserID: "owner-1", Status: "suspended"},
			node:    &models.Node{ID: 5, Token: "node-tok"},
		}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "root", true, "admin1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("admin override is recorded in the power-action audit", func(t *testing.T) {
		fs := &serverPowerFakeStore{
			server:       &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerName: "alice", OwnerID: "owner-1", NodeID: 5},
			billing:      &store.UserBilling{UserID: "owner-1", Status: "suspended"},
			node:         &models.Node{ID: 5, Token: "node-tok"},
			auditEnabled: true,
		}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "root", true, "admin1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.insertedAudits) != 1 {
			t.Fatalf("want 1 audit event, got %d", len(fs.insertedAudits))
		}
		ev := fs.insertedAudits[0]
		if ev.EventType != ServerAuditEventPowerAction {
			t.Errorf("event type = %q, want %q", ev.EventType, ServerAuditEventPowerAction)
		}
		if ev.Metadata["admin_suspend_override"] != "owner_billing_suspended" {
			t.Errorf("override marker = %v, want owner_billing_suspended", ev.Metadata["admin_suspend_override"])
		}
	})

	t.Run("no override marker on a normal non-suspended admin start", func(t *testing.T) {
		fs := &serverPowerFakeStore{
			server:       &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerName: "alice", OwnerID: "owner-1", NodeID: 5},
			node:         &models.Node{ID: 5, Token: "node-tok"},
			auditEnabled: true,
		}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "root", true, "admin1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.insertedAudits) != 1 {
			t.Fatalf("want 1 audit event, got %d", len(fs.insertedAudits))
		}
		if _, ok := fs.insertedAudits[0].Metadata["admin_suspend_override"]; ok {
			t.Error("no override marker expected for a non-suspended start")
		}
	})
}

// --- Install cooldown (miniredis TTL key) ---

func TestServerPowerHandler_InstallCooldown(t *testing.T) {
	t.Run("non-admin blocked while cooldown key has TTL", func(t *testing.T) {
		fs := &serverPowerFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerName: "alice", OwnerID: "u1"}}
		rdb := newServerPowerRedis(t)
		if err := rdb.Set(context.Background(), "dylaris:server:srv-uuid:install-start", "1", 30*time.Second).Err(); err != nil {
			t.Fatalf("seed cooldown key: %v", err)
		}
		h := newServerPowerHandler(fs, rdb)
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "alice", false, "u1"))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("admin bypasses the cooldown", func(t *testing.T) {
		fs := &serverPowerFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerName: "alice", NodeID: 5}, node: &models.Node{ID: 5, Token: "node-tok"}}
		rdb := newServerPowerRedis(t)
		if err := rdb.Set(context.Background(), "dylaris:server:srv-uuid:install-start", "1", 30*time.Second).Err(); err != nil {
			t.Fatalf("seed cooldown key: %v", err)
		}
		h := newServerPowerHandler(fs, rdb)
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "root", true, "admin1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("no cooldown key present, not blocked", func(t *testing.T) {
		fs := &serverPowerFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerName: "alice", OwnerID: "u1", NodeID: 5}, node: &models.Node{ID: 5, Token: "node-tok"}}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "start", "alice", false, "u1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})
}

// --- The isOffline/isOnline x action 409 status-transition matrix (crown jewel) ---

func TestServerPowerHandler_StatusTransitionMatrix(t *testing.T) {
	cases := []struct {
		status     string
		action     string
		wantStatus int
	}{
		{"online", "start", http.StatusConflict}, // already running
		{"stopped", "start", http.StatusOK},      // happy path
		{"offline", "start", http.StatusOK},
		{"online", "stop", http.StatusOK},        // happy path
		{"stopped", "stop", http.StatusConflict}, // not running
		{"offline", "stop", http.StatusConflict},
		{"online", "kill", http.StatusOK},
		{"stopped", "kill", http.StatusConflict},
		{"online", "restart", http.StatusOK},
		{"stopped", "restart", http.StatusConflict},
		{"offline", "restart", http.StatusConflict},
	}
	for _, c := range cases {
		t.Run(c.status+"_"+c.action, func(t *testing.T) {
			fs := &serverPowerFakeStore{
				server: &models.Server{ID: 1, UUID: "srv-uuid", Status: c.status, OwnerName: "alice", OwnerID: "u1", NodeID: 5},
				node:   &models.Node{ID: 5, Token: "node-tok"},
			}
			h := newServerPowerHandler(fs, newServerPowerRedis(t))
			rec := httptest.NewRecorder()
			h.ServerPowerHandler(rec, serverPowerReq(1, c.action, "alice", false, "u1"))
			if rec.Code != c.wantStatus {
				t.Fatalf("status(%s on %s) = %d, want %d: %s", c.action, c.status, rec.Code, c.wantStatus, rec.Body.String())
			}
			if c.wantStatus == http.StatusConflict {
				if msg := decodeErrBody(t, rec); c.action == "start" {
					if msg != "Server is already running" {
						t.Fatalf("message = %q", msg)
					}
				} else if msg != "Server is not running" {
					t.Fatalf("message = %q", msg)
				}
			}
		})
	}
}

func TestServerPowerHandler_NodeNotFound(t *testing.T) {
	fs := &serverPowerFakeStore{
		server:  &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerName: "alice", NodeID: 5},
		nodeErr: errors.New("no node"),
	}
	h := newServerPowerHandler(fs, newServerPowerRedis(t))
	rec := httptest.NewRecorder()
	h.ServerPowerHandler(rec, serverPowerReq(1, "start", "root", true, "admin1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// --- Happy path: assert the exact queued command + desired-state/status writes ---

func readNodeCmdStream(t *testing.T, rdb *redis.Client, stream string) map[string]interface{} {
	t.Helper()
	msgs, err := rdb.XRange(context.Background(), stream, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange %s: %v", stream, err)
	}
	if len(msgs) != 1 {
		t.Fatalf("stream %s: got %d entries, want 1", stream, len(msgs))
	}
	raw, ok := msgs[0].Values["data"].(string)
	if !ok {
		t.Fatalf("stream %s: entry has no string data field: %+v", stream, msgs[0].Values)
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("stream %s: unmarshal payload: %v", stream, err)
	}
	return out
}

func TestServerPowerHandler_HappyPath_QueuesCommandAndUpdatesState(t *testing.T) {
	cases := []struct {
		action           string
		startStatus      string
		wantNewStatus    string
		wantDesiredState string
	}{
		{"start", "stopped", "starting", "online"},
		{"restart", "online", "starting", "online"},
		{"stop", "online", "stopping", "stopped"},
		{"kill", "online", "stopping", "stopped"},
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			fs := &serverPowerFakeStore{
				server: &models.Server{ID: 42, UUID: "srv-uuid-42", Status: c.startStatus, OwnerName: "alice", OwnerID: "u1", NodeID: 5},
				node:   &models.Node{ID: 5, Token: "node-tok-42"},
			}
			rdb := newServerPowerRedis(t)
			h := newServerPowerHandler(fs, rdb)
			rec := httptest.NewRecorder()
			h.ServerPowerHandler(rec, serverPowerReq(42, c.action, "alice", false, "u1"))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if !resp.Success || resp.Message != "Action "+c.action+" queued successfully" {
				t.Fatalf("response = %+v", resp)
			}

			if len(fs.updateStatusCalls) != 1 || fs.updateStatusCalls[0] != (serverPowerStatusCall{42, c.wantNewStatus}) {
				t.Fatalf("updateStatusCalls = %+v, want [{42 %s}]", fs.updateStatusCalls, c.wantNewStatus)
			}
			if len(fs.updateDesiredStateCalls) != 1 || fs.updateDesiredStateCalls[0] != (serverPowerDesiredStateCall{42, c.wantDesiredState}) {
				t.Fatalf("updateDesiredStateCalls = %+v, want [{42 %s}]", fs.updateDesiredStateCalls, c.wantDesiredState)
			}

			payload := readNodeCmdStream(t, rdb, "dylaris:node:node-tok-42:cmds")
			if payload["action"] != c.action {
				t.Fatalf("queued action = %v, want %v", payload["action"], c.action)
			}
			cfg, ok := payload["config"].(map[string]interface{})
			if !ok || cfg["uuid"] != "srv-uuid-42" {
				t.Fatalf("queued config = %+v, want uuid=srv-uuid-42", payload["config"])
			}
		})
	}
}

// TestServerPowerHandler_QueueSendCommandFailure points Queue at an address
// with nothing listening so SendCommand's XADD fails, pinning the 500 path
// (servers_lifecycle.go:793-797). A short DialTimeout keeps the test fast.
func TestServerPowerHandler_QueueSendCommandFailure(t *testing.T) {
	fs := &serverPowerFakeStore{
		server: &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerName: "alice", OwnerID: "u1", NodeID: 5},
		node:   &models.Node{ID: 5, Token: "node-tok"},
	}
	brokenRDB := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 300 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { brokenRDB.Close() })
	h := &ServerHandler{state: &AppState{
		Store: fs,
		Redis: newServerPowerRedis(t), // cooldown check needs a working Redis
		Queue: services.NewQueueService(brokenRDB),
		Authz: authz.NewResolver(fs),
	}}
	rec := httptest.NewRecorder()
	h.ServerPowerHandler(rec, serverPowerReq(1, "start", "alice", false, "u1"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeErrBody(t, rec); msg != "Failed to queue command" {
		t.Fatalf("message = %q, want %q", msg, "Failed to queue command")
	}
}

// --- byonNodeServerCapReached ---

func TestByonNodeServerCapReached(t *testing.T) {
	t.Run("under the fallback capN (unknown topology) is not reached", func(t *testing.T) {
		fs := &serverPowerFakeStore{countServersByNode: 3}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		reached, capN := h.byonNodeServerCapReached(context.Background(), &models.Node{ID: 1, Token: "tok"})
		if reached || capN != byonNodeServerFallbackCap {
			t.Fatalf("reached=%v capN=%d, want false/%d", reached, capN, byonNodeServerFallbackCap)
		}
	})

	t.Run("at the fallback capN is reached", func(t *testing.T) {
		fs := &serverPowerFakeStore{countServersByNode: byonNodeServerFallbackCap}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		reached, capN := h.byonNodeServerCapReached(context.Background(), &models.Node{ID: 1, Token: "tok"})
		if !reached || capN != byonNodeServerFallbackCap {
			t.Fatalf("reached=%v capN=%d, want true/%d", reached, capN, byonNodeServerFallbackCap)
		}
	})

	t.Run("over the fallback capN is reached", func(t *testing.T) {
		fs := &serverPowerFakeStore{countServersByNode: byonNodeServerFallbackCap + 5}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		reached, _ := h.byonNodeServerCapReached(context.Background(), &models.Node{ID: 1, Token: "tok"})
		if !reached {
			t.Fatalf("reached = false, want true")
		}
	})

	t.Run("store error on CountServersByNode fails open (never blocks)", func(t *testing.T) {
		fs := &serverPowerFakeStore{countServersByNodeErr: errors.New("db down")}
		h := newServerPowerHandler(fs, newServerPowerRedis(t))
		reached, capN := h.byonNodeServerCapReached(context.Background(), &models.Node{ID: 1, Token: "tok"})
		if reached || capN != byonNodeServerFallbackCap {
			t.Fatalf("reached=%v capN=%d, want false/%d (fail-open)", reached, capN, byonNodeServerFallbackCap)
		}
	})

	// With a real CPUPinningService wired to miniredis and a published topology,
	// the capN becomes factor x logical cores instead of the fallback ceiling.
	t.Run("known topology uses factor x cores instead of the fallback", func(t *testing.T) {
		rdb := newServerPowerRedis(t)
		fs := &serverPowerFakeStore{
			settings:           map[string]string{"byon.max_servers_per_core": "3"},
			countServersByNode: 11,
		}
		topo := services.CPUTopology{LogicalCount: 4}
		raw, err := json.Marshal(topo)
		if err != nil {
			t.Fatalf("marshal topology: %v", err)
		}
		if err := rdb.Set(context.Background(), "dylaris:node:node-tok:cpu", raw, 0).Err(); err != nil {
			t.Fatalf("seed topology: %v", err)
		}
		h := &ServerHandler{state: &AppState{
			Store:      fs,
			Redis:      rdb,
			CPUPinning: services.NewCPUPinningService(rdb, fs),
		}}
		// capN = 3 * 4 = 12; count=11 -> not reached
		reached, capN := h.byonNodeServerCapReached(context.Background(), &models.Node{ID: 1, Token: "node-tok"})
		if reached || capN != 12 {
			t.Fatalf("reached=%v capN=%d, want false/12", reached, capN)
		}
		fs.countServersByNode = 12
		reached, capN = h.byonNodeServerCapReached(context.Background(), &models.Node{ID: 1, Token: "node-tok"})
		if !reached || capN != 12 {
			t.Fatalf("reached=%v capN=%d, want true/12", reached, capN)
		}
	})
}
