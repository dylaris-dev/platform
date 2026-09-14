package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// grantFakeStore embeds store.Store (nil). It backs BOTH the assign handler and
// the Phase-1 resolver (authz.NewResolver(fake)): it overrides GetUserByUsername
// + GetServerByID for the handler and the resolver read methods for the actor's
// cap resolution. UpsertServerGrant / DeleteServerGrant record their calls.
type grantFakeStore struct {
	store.Store
	target      *models.User
	server      *models.Server
	actorGrant  *store.ServerGrant // returned by GetServerGrant for the actor
	serverRole  *store.ServerRole
	upserts     []upsertCall
	deleteCalls int
	deleteErr   error
	grants      []store.OwnerGrant
	mode        string
	node        *models.Node // the server's node; nil = a platform node
	byon        string       // feature_byon_enabled; "" = never saved

	auditEnabled     bool
	auditEnableCalls int
	serverAudit      []models.ServerAuditEvent
	identityAudit    []models.AuditEventIdentity
}

func (f *grantFakeStore) GetSetting(key string) (string, error) {
	if key == "feature_byon_enabled" {
		return f.byon, nil
	}
	return f.mode, nil
}

type upsertCall struct {
	serverID     *int
	userID       string
	ownerUserID  string
	serverRoleID *int
	overrides    store.CapOverrides
	inherit      bool
}

func (f *grantFakeStore) GetUserByUsername(string) (*models.User, error) {
	if f.target == nil {
		return nil, sql.ErrNoRows
	}
	return f.target, nil
}

// GetNodeByID answers node, or a platform node when node is nil.
func (f *grantFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.node != nil {
		return f.node, nil
	}
	return &models.Node{ID: id}, nil
}

func (f *grantFakeStore) GetServerByID(int) (*models.Server, error) {
	if f.server == nil {
		return nil, sql.ErrNoRows
	}
	return f.server, nil
}
func (f *grantFakeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}
func (f *grantFakeStore) GetServerGrant(int, string) (*store.ServerGrant, error) {
	if f.actorGrant == nil {
		return nil, sql.ErrNoRows
	}
	return f.actorGrant, nil
}
func (f *grantFakeStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	return nil, sql.ErrNoRows
}
func (f *grantFakeStore) GetServerRole(int) (*store.ServerRole, error) {
	if f.serverRole == nil {
		return nil, sql.ErrNoRows
	}
	return f.serverRole, nil
}
func (f *grantFakeStore) UpsertServerGrant(serverID *int, userID, ownerUserID string, serverRoleID *int, overrides store.CapOverrides, inherit bool) error {
	f.upserts = append(f.upserts, upsertCall{serverID, userID, ownerUserID, serverRoleID, overrides, inherit})
	return nil
}
func (f *grantFakeStore) DeleteServerGrant(*int, string, string) error {
	f.deleteCalls++
	return f.deleteErr
}
func (f *grantFakeStore) ListGrantsByOwner(string) ([]store.OwnerGrant, error) {
	return f.grants, nil
}

// The audit side. Handing someone access to a server had no audit row at all
// through this route, so these exist to make the rows observable - and to keep
// the handler from reaching the nil embedded store now that it writes them.
func (f *grantFakeStore) GetServerAuditState(int) (bool, bool, int, error) {
	return f.auditEnabled, false, len(f.serverAudit), nil
}
func (f *grantFakeStore) SetServerAuditEnabled(_ int, enabled bool) error {
	f.auditEnabled = enabled
	f.auditEnableCalls++
	return nil
}
func (f *grantFakeStore) InsertServerAudit(ev *models.ServerAuditEvent) error {
	f.serverAudit = append(f.serverAudit, *ev)
	return nil
}
func (f *grantFakeStore) InsertAuditIdentity(ev *models.AuditEventIdentity) error {
	f.identityAudit = append(f.identityAudit, *ev)
	return nil
}

func grantState(fs *grantFakeStore) *AppState {
	return &AppState{Store: fs, Authz: authz.NewResolver(fs)}
}

func grantReq(method string, actorID string, isAdmin bool, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, "/api/grants", &buf)
	ctx := context.WithValue(r.Context(), "userID", actorID)
	ctx = context.WithValue(ctx, "username", "actor")
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	return r.WithContext(ctx)
}

const (
	ownerA  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" // server owner
	actorB  = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" // non-owner assigner
	friendC = "cccccccc-cccc-cccc-cccc-cccccccccccc" // target friend
)

func serverOwnedBy(owner string) *models.Server {
	return &models.Server{ID: 42, OwnerID: owner, OwnerName: "ownerA"}
}

// Non-owner holding members.write but NOT files.delete cannot grant files.delete.
func TestAssignGrant_NonOwnerCannotGrantCapTheyLack(t *testing.T) {
	sid := 42
	fs := &grantFakeStore{
		target: &models.User{ID: friendC, Username: "friend"},
		server: serverOwnedBy(ownerA),
		actorGrant: &store.ServerGrant{
			ServerID:     &sid,
			UserID:       actorB,
			CapOverrides: store.CapOverrides{Grant: []string{"members.write", "files.read"}},
		},
	}
	h := NewServerRolesHandler(grantState(fs))
	rec := httptest.NewRecorder()
	h.AssignGrant(rec, grantReq("POST", actorB, false, map[string]interface{}{
		"username": "friend", "serverId": 42, "grantCaps": []string{"files.delete"},
	}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if len(fs.upserts) != 0 {
		t.Fatalf("expected no upsert, got %d", len(fs.upserts))
	}
}

// Non-owner with members.write cannot smuggle a cap they lack by REFERENCING a
// server-role that contains it (roleCaps are folded into the delegation subset check).
func TestAssignGrant_NonOwnerCannotSmuggleCapViaRole(t *testing.T) {
	sid := 42
	fs := &grantFakeStore{
		target:     &models.User{ID: friendC, Username: "friend"},
		server:     serverOwnedBy(ownerA),
		serverRole: &store.ServerRole{ID: 7, OwnerUserID: ownerA, Capabilities: []string{"files.delete"}},
		actorGrant: &store.ServerGrant{
			ServerID:     &sid,
			UserID:       actorB,
			CapOverrides: store.CapOverrides{Grant: []string{"members.write", "files.read"}},
		},
	}
	h := NewServerRolesHandler(grantState(fs))
	rec := httptest.NewRecorder()
	h.AssignGrant(rec, grantReq("POST", actorB, false, map[string]interface{}{
		"username": "friend", "serverId": 42, "serverRoleId": 7,
	}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if len(fs.upserts) != 0 {
		t.Fatalf("expected no upsert, got %d", len(fs.upserts))
	}
}

// Non-owner holding members.write AND files.delete CAN grant files.delete.
func TestAssignGrant_NonOwnerCanGrantHeldCap(t *testing.T) {
	sid := 42
	fs := &grantFakeStore{
		target: &models.User{ID: friendC, Username: "friend"},
		server: serverOwnedBy(ownerA),
		actorGrant: &store.ServerGrant{
			ServerID:     &sid,
			UserID:       actorB,
			CapOverrides: store.CapOverrides{Grant: []string{"members.write", "files.delete"}},
		},
	}
	h := NewServerRolesHandler(grantState(fs))
	rec := httptest.NewRecorder()
	h.AssignGrant(rec, grantReq("POST", actorB, false, map[string]interface{}{
		"username": "friend", "serverId": 42, "grantCaps": []string{"files.delete"},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.upserts) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(fs.upserts))
	}
	if fs.upserts[0].ownerUserID != ownerA || fs.upserts[0].serverID == nil || *fs.upserts[0].serverID != 42 {
		t.Fatalf("upsert unexpected: %+v", fs.upserts[0])
	}
}

// The server owner bypasses the delegation cap entirely.
func TestAssignGrant_OwnerBypassesDelegation(t *testing.T) {
	fs := &grantFakeStore{
		target: &models.User{ID: friendC, Username: "friend"},
		server: serverOwnedBy(ownerA), // actor IS ownerA
	}
	h := NewServerRolesHandler(grantState(fs))
	rec := httptest.NewRecorder()
	h.AssignGrant(rec, grantReq("POST", ownerA, false, map[string]interface{}{
		"username": "friend", "serverId": 42, "grantCaps": []string{"files.delete"},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.upserts) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(fs.upserts))
	}
}

// An admin bypasses the delegation cap on any realm.
func TestAssignGrant_AdminBypassesDelegation(t *testing.T) {
	fs := &grantFakeStore{
		target: &models.User{ID: friendC, Username: "friend"},
		server: serverOwnedBy(ownerA),
	}
	h := NewServerRolesHandler(grantState(fs))
	rec := httptest.NewRecorder()
	h.AssignGrant(rec, grantReq("POST", actorB, true, map[string]interface{}{
		"username": "friend", "serverId": 42, "grantCaps": []string{"files.delete"},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.upserts) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(fs.upserts))
	}
}

// A PANEL-scope cap is rejected before any store call.
func TestAssignGrant_RejectsPanelCap(t *testing.T) {
	fs := &grantFakeStore{target: &models.User{ID: friendC, Username: "friend"}, server: serverOwnedBy(ownerA)}
	h := NewServerRolesHandler(grantState(fs))
	rec := httptest.NewRecorder()
	h.AssignGrant(rec, grantReq("POST", ownerA, false, map[string]interface{}{
		"username": "friend", "serverId": 42, "grantCaps": []string{"users.read"},
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if len(fs.upserts) != 0 {
		t.Fatalf("expected no upsert, got %d", len(fs.upserts))
	}
}

// Account-wide assign targets the acting user's own realm (ownerUserID = actor).
func TestAssignGrant_AccountWideOwnRealm(t *testing.T) {
	fs := &grantFakeStore{target: &models.User{ID: friendC, Username: "friend"}}
	h := NewServerRolesHandler(grantState(fs))
	rec := httptest.NewRecorder()
	h.AssignGrant(rec, grantReq("POST", actorB, false, map[string]interface{}{
		"username": "friend", "grantCaps": []string{"modpack.read"},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.upserts) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(fs.upserts))
	}
	if fs.upserts[0].serverID != nil {
		t.Fatalf("account-wide upsert must have nil serverID, got %v", *fs.upserts[0].serverID)
	}
	if fs.upserts[0].ownerUserID != actorB {
		t.Fatalf("account-wide ownerUserID = %q, want acting user %q", fs.upserts[0].ownerUserID, actorB)
	}
}

// Revoke by the owner removes the per-server grant.
func TestRevokeGrant_OwnerRemoves(t *testing.T) {
	fs := &grantFakeStore{target: &models.User{ID: friendC, Username: "friend"}, server: serverOwnedBy(ownerA)}
	h := NewServerRolesHandler(grantState(fs))
	rec := httptest.NewRecorder()
	h.RevokeGrant(rec, grantReq("DELETE", ownerA, false, map[string]interface{}{
		"username": "friend", "serverId": 42,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if fs.deleteCalls != 1 {
		t.Fatalf("expected 1 DeleteServerGrant call, got %d", fs.deleteCalls)
	}
}

func TestListGrants_ReturnsOwnerRealmGrants(t *testing.T) {
	sid := 42
	fs := &grantFakeStore{grants: []store.OwnerGrant{
		{Username: "friend", ServerID: &sid, ServerName: "Survival", ServerRoleName: "Builders",
			CapOverrides: store.CapOverrides{Grant: []string{"files.read"}}},
		{Username: "buddy", ServerID: nil,
			CapOverrides: store.CapOverrides{Deny: []string{"backups.delete"}}, Inherit: true},
	}}
	h := NewServerRolesHandler(grantState(fs))
	rec := httptest.NewRecorder()
	h.ListGrants(rec, grantReq("GET", ownerA, false, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Grants  []struct {
			Username    string `json:"username"`
			ServerName  string `json:"serverName"`
			AccountWide bool   `json:"accountWide"`
		} `json:"grants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || len(resp.Grants) != 2 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Grants[0].Username != "friend" || resp.Grants[0].ServerName != "Survival" || resp.Grants[0].AccountWide {
		t.Fatalf("grant[0] = %+v", resp.Grants[0])
	}
	if !resp.Grants[1].AccountWide {
		t.Fatalf("grant[1] should be account-wide: %+v", resp.Grants[1])
	}
}

func TestAssignGrant_ModeGate(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		isAdmin    bool
		wantCode   int
		wantUpsert bool
	}{
		{"advanced allowed", "advanced", false, http.StatusOK, true},
		{"simple allowed", "simple", false, http.StatusOK, true},
		{"off blocked", "off", false, http.StatusForbidden, false},
		{"off admin bypass", "off", true, http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &grantFakeStore{target: &models.User{ID: friendC, Username: "friend"}, mode: tc.mode}
			h := NewServerRolesHandler(grantState(fs))
			rec := httptest.NewRecorder()
			h.AssignGrant(rec, grantReq("POST", ownerA, tc.isAdmin, map[string]interface{}{
				"username": "friend", "grantCaps": []string{"modpack.read"},
			}))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if (len(fs.upserts) == 1) != tc.wantUpsert {
				t.Fatalf("upserts = %d, wantUpsert %v", len(fs.upserts), tc.wantUpsert)
			}
		})
	}
}

// Handing someone access to a server is the most security-relevant thing that
// happens to one, and through this route it recorded nothing. The three member_*
// events existed but were wired only to POST /servers/{id}/members - the legacy
// route the panel does not call. Measured live before the fix: granting a
// stranger files.delete + sftp.access left the audit row count unchanged.
func TestAssignGrant_IsAudited(t *testing.T) {
	t.Run("a first per-server grant reads as an invite", func(t *testing.T) {
		fs := &grantFakeStore{
			target: &models.User{ID: friendC, Username: "friend"},
			server: serverOwnedBy(ownerA),
		}
		h := NewServerRolesHandler(grantState(fs))
		rec := httptest.NewRecorder()
		h.AssignGrant(rec, grantReq("POST", ownerA, false, map[string]interface{}{
			"username": "friend", "serverId": 42, "grantCaps": []string{"files.delete", "sftp.access"},
		}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if len(fs.serverAudit) != 1 {
			t.Fatalf("audit rows = %d, want 1: this is the gap - the grant was invisible", len(fs.serverAudit))
		}
		ev := fs.serverAudit[0]
		if ev.EventType != ServerAuditEventMemberInvited {
			t.Errorf("eventType = %q, want %q", ev.EventType, ServerAuditEventMemberInvited)
		}
		if ev.ServerID != 42 {
			t.Errorf("serverId = %d, want 42", ev.ServerID)
		}
		if fs.auditEnableCalls != 1 {
			t.Error("auditing was not switched on: on a server whose access was only ever granted here, the events had nowhere to land")
		}
	})

	t.Run("a second grant to the same person reads as a change", func(t *testing.T) {
		sid := 42
		fs := &grantFakeStore{
			target:     &models.User{ID: friendC, Username: "friend"},
			server:     serverOwnedBy(ownerA),
			actorGrant: &store.ServerGrant{ServerID: &sid, UserID: friendC},
		}
		h := NewServerRolesHandler(grantState(fs))
		rec := httptest.NewRecorder()
		h.AssignGrant(rec, grantReq("POST", ownerA, false, map[string]interface{}{
			"username": "friend", "serverId": 42, "grantCaps": []string{"console.read"},
		}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if len(fs.serverAudit) != 1 || fs.serverAudit[0].EventType != ServerAuditEventMemberPermsChanged {
			t.Fatalf("audit = %+v, want one %s", fs.serverAudit, ServerAuditEventMemberPermsChanged)
		}
	})

	t.Run("an account-wide grant lands in the identity log", func(t *testing.T) {
		// It has no single server to belong to, and is the more powerful of the
		// two shapes - every server in the realm, present and future.
		fs := &grantFakeStore{target: &models.User{ID: friendC, Username: "friend"}}
		h := NewServerRolesHandler(grantState(fs))
		rec := httptest.NewRecorder()
		h.AssignGrant(rec, grantReq("POST", ownerA, false, map[string]interface{}{
			"username": "friend", "grantCaps": []string{"files.delete"},
		}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if len(fs.serverAudit) != 0 {
			t.Errorf("a server audit row was written for a grant that names no server: %+v", fs.serverAudit)
		}
		if len(fs.identityAudit) != 1 || fs.identityAudit[0].EventType != AuditEventAccountGrantAssigned {
			t.Fatalf("identity audit = %+v, want one %s", fs.identityAudit, AuditEventAccountGrantAssigned)
		}
	})

	t.Run("a refused grant records nothing", func(t *testing.T) {
		sid := 42
		fs := &grantFakeStore{
			target: &models.User{ID: friendC, Username: "friend"},
			server: serverOwnedBy(ownerA),
			actorGrant: &store.ServerGrant{
				ServerID: &sid, UserID: actorB,
				CapOverrides: store.CapOverrides{Grant: []string{"members.write"}},
			},
		}
		h := NewServerRolesHandler(grantState(fs))
		rec := httptest.NewRecorder()
		h.AssignGrant(rec, grantReq("POST", actorB, false, map[string]interface{}{
			"username": "friend", "serverId": 42, "grantCaps": []string{"files.delete"},
		}))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if len(fs.serverAudit) != 0 || len(fs.identityAudit) != 0 {
			t.Error("an audit row was written for a grant that never happened")
		}
	})
}

// The other half of "when did they get it".
//
// auditEnabled is set here rather than left to the handler: LogServerAudit only
// writes when the server has auditing on, and a revoke never turns it on -
// deliberately, since the trigger for switching it on is somebody GAINING
// access. In reality the matching assign has already done that.
func TestRevokeGrant_IsAudited(t *testing.T) {
	t.Run("per-server", func(t *testing.T) {
		fs := &grantFakeStore{
			target:       &models.User{ID: friendC, Username: "friend"},
			server:       serverOwnedBy(ownerA),
			auditEnabled: true,
		}
		h := NewServerRolesHandler(grantState(fs))
		rec := httptest.NewRecorder()
		h.RevokeGrant(rec, grantReq("DELETE", ownerA, false, map[string]interface{}{
			"username": "friend", "serverId": 42,
		}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if len(fs.serverAudit) != 1 || fs.serverAudit[0].EventType != ServerAuditEventMemberRemoved {
			t.Fatalf("audit = %+v, want one %s", fs.serverAudit, ServerAuditEventMemberRemoved)
		}
	})

	t.Run("account-wide", func(t *testing.T) {
		fs := &grantFakeStore{target: &models.User{ID: friendC, Username: "friend"}}
		h := NewServerRolesHandler(grantState(fs))
		rec := httptest.NewRecorder()
		h.RevokeGrant(rec, grantReq("DELETE", ownerA, false, map[string]interface{}{"username": "friend"}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if len(fs.identityAudit) != 1 || fs.identityAudit[0].EventType != AuditEventAccountGrantRevoked {
			t.Fatalf("identity audit = %+v, want one %s", fs.identityAudit, AuditEventAccountGrantRevoked)
		}
	})
}

// An admin writes grants in a customer's name, so on a customer's machine the
// admin flag must not skip the delegation cap: roles.write resolved them as an
// admin (it is an OWNER capability, asked with no server), and with the bypass
// an admin could invite themselves or a throwaway account onto any server.
func TestAGrantOnACustomersMachineIsNotAnAdminBypass(t *testing.T) {
	owner := ownerA
	customerNode := &models.Node{ID: 9, OwnerID: &owner}
	for _, c := range []struct {
		name    string
		byon    string
		refused bool
	}{
		{"BYON on", "true", true},
		// With BYON off an owner_id carries no tenancy meaning (decision D6 is
		// open), so the admin bypass stays, as for every other fence.
		{"BYON never saved", "", false},
	} {
		t.Run("assign/"+c.name, func(t *testing.T) {
			fs := &grantFakeStore{target: &models.User{ID: friendC, Username: "friend"}, server: serverOwnedBy(ownerA), node: customerNode, byon: c.byon}
			rec := httptest.NewRecorder()
			NewServerRolesHandler(fencedGrantState(fs)).AssignGrant(rec, grantReq("POST", actorB, true, map[string]interface{}{
				"username": "friend", "serverId": 42, "grantCaps": []string{"files.read"},
			}))
			if refused := rec.Code == http.StatusForbidden && len(fs.upserts) == 0; refused != c.refused {
				t.Fatalf("refused = %v, want %v (status %d, upserts %d)", refused, c.refused, rec.Code, len(fs.upserts))
			}
		})
		t.Run("revoke/"+c.name, func(t *testing.T) {
			fs := &grantFakeStore{target: &models.User{ID: friendC, Username: "friend"}, server: serverOwnedBy(ownerA), node: customerNode, byon: c.byon}
			rec := httptest.NewRecorder()
			NewServerRolesHandler(fencedGrantState(fs)).RevokeGrant(rec, grantReq("DELETE", actorB, true, map[string]interface{}{
				"username": "friend", "serverId": 42,
			}))
			if refused := rec.Code == http.StatusForbidden && fs.deleteCalls == 0; refused != c.refused {
				t.Fatalf("refused = %v, want %v (status %d, deletes %d)", refused, c.refused, rec.Code, fs.deleteCalls)
			}
		})
	}
}

// fencedGrantState wires the resolver the way main does, so an admin who falls
// back to the delegation cap is resolved with the same fence.
func fencedGrantState(fs *grantFakeStore) *AppState {
	st := grantState(fs)
	st.FeatureFlags = services.NewFeatureFlags(fs)
	st.Authz.SetForeignNode(st.NodeOwnedByOther)
	return st
}
