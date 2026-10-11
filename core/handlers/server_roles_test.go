package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/store"
)

// serverRolesFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the server-role methods are overridden. Any
// other call panics - these tests never make one.
type serverRolesFakeStore struct {
	store.Store
	roles       []store.ServerRole
	createID    int
	createErr   error
	createOwner string
	createCalls int
	updateCalls int
	updateOwner string
	deleteCalls int
	deleteOwner string
	mode        string
	getRole     *store.ServerRole
	audits      []*models.AuditEventIdentity
}

func (f *serverRolesFakeStore) GetServerRole(int) (*store.ServerRole, error) { return f.getRole, nil }
func (f *serverRolesFakeStore) InsertAuditIdentity(ev *models.AuditEventIdentity) error {
	f.audits = append(f.audits, ev)
	return nil
}

func (f *serverRolesFakeStore) GetSetting(string) (string, error) {
	return f.mode, nil
}

func (f *serverRolesFakeStore) ListServerRolesByOwner(string) ([]store.ServerRole, error) {
	return f.roles, nil
}
func (f *serverRolesFakeStore) CreateServerRole(ownerUserID, name string, caps []string) (int, error) {
	f.createCalls++
	f.createOwner = ownerUserID
	return f.createID, f.createErr
}
func (f *serverRolesFakeStore) UpdateServerRole(id int, ownerUserID, name string, caps []string) error {
	f.updateCalls++
	f.updateOwner = ownerUserID
	return nil
}
func (f *serverRolesFakeStore) DeleteServerRole(id int, ownerUserID string) error {
	f.deleteCalls++
	f.deleteOwner = ownerUserID
	return nil
}

const testActorID = "11111111-1111-1111-1111-111111111111"

func serverRoleReq(method, target string, vars map[string]string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, target, &buf)
	if vars != nil {
		r = mux.SetURLVars(r, vars)
	}
	ctx := context.WithValue(r.Context(), "userID", testActorID)
	ctx = context.WithValue(ctx, "username", "actor")
	ctx = context.WithValue(ctx, "isAdmin", false)
	return r.WithContext(ctx)
}

func TestServerRoles_CreateRejectsPanelCap(t *testing.T) {
	// users.read is a PANEL-scope cap; a server-role must reject it.
	// mode: advanced so the request reaches cap validation, not the mode gate.
	fs := &serverRolesFakeStore{mode: "advanced"}
	h := NewServerRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.CreateServerRole(rec, serverRoleReq("POST", "/api/server-roles", nil,
		map[string]interface{}{"name": "x", "capabilities": []string{"users.read"}}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if fs.createCalls != 0 {
		t.Fatalf("expected no CreateServerRole call, got %d", fs.createCalls)
	}
}

func TestServerRoles_CreateRejectsUnknownCap(t *testing.T) {
	// mode: advanced so the request reaches cap validation, not the mode gate.
	fs := &serverRolesFakeStore{mode: "advanced"}
	h := NewServerRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.CreateServerRole(rec, serverRoleReq("POST", "/api/server-roles", nil,
		map[string]interface{}{"name": "x", "capabilities": []string{"does.not.exist"}}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if fs.createCalls != 0 {
		t.Fatalf("expected no CreateServerRole call, got %d", fs.createCalls)
	}
}

func TestServerRoles_CreateAcceptsServerAndOwnerCaps(t *testing.T) {
	fs := &serverRolesFakeStore{createID: 12, mode: "advanced"}
	h := NewServerRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.CreateServerRole(rec, serverRoleReq("POST", "/api/server-roles", nil,
		map[string]interface{}{"name": "Builders", "capabilities": []string{"files.read", "modpack.read"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if fs.createCalls != 1 {
		t.Fatalf("expected 1 CreateServerRole call, got %d", fs.createCalls)
	}
	if fs.createOwner != testActorID {
		t.Fatalf("CreateServerRole owner = %q, want acting user %q", fs.createOwner, testActorID)
	}
	var resp struct {
		Success bool `json:"success"`
		Role    struct {
			ID           int      `json:"id"`
			Capabilities []string `json:"capabilities"`
		} `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || resp.Role.ID != 12 || len(resp.Role.Capabilities) != 2 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestServerRoles_UpdateScopesToActingOwner(t *testing.T) {
	fs := &serverRolesFakeStore{mode: "advanced"}
	h := NewServerRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.UpdateServerRole(rec, serverRoleReq("PATCH", "/api/server-roles/2", map[string]string{"id": "2"},
		map[string]interface{}{"name": "Builders2", "capabilities": []string{"files.read"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if fs.updateCalls != 1 || fs.updateOwner != testActorID {
		t.Fatalf("UpdateServerRole calls=%d owner=%q, want 1 / %q", fs.updateCalls, fs.updateOwner, testActorID)
	}
}

func TestServerRoles_DeleteScopesToActingOwner(t *testing.T) {
	fs := &serverRolesFakeStore{mode: "advanced"}
	h := NewServerRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.DeleteServerRole(rec, serverRoleReq("DELETE", "/api/server-roles/3", map[string]string{"id": "3"}, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if fs.deleteCalls != 1 || fs.deleteOwner != testActorID {
		t.Fatalf("DeleteServerRole calls=%d owner=%q, want 1 / %q", fs.deleteCalls, fs.deleteOwner, testActorID)
	}
}

func TestServerRoles_ListReturnsOwnRoles(t *testing.T) {
	fs := &serverRolesFakeStore{roles: []store.ServerRole{
		{ID: 1, OwnerUserID: testActorID, Name: "Builders", Capabilities: []string{"files.read"}},
	}}
	h := NewServerRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.ListServerRoles(rec, serverRoleReq("GET", "/api/server-roles", nil, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Roles   []struct {
			Name string `json:"name"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || len(resp.Roles) != 1 || resp.Roles[0].Name != "Builders" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func serverRoleReqAdmin(method, target string, vars map[string]string, isAdmin bool, body interface{}) *http.Request {
	r := serverRoleReq(method, target, vars, body)
	return r.WithContext(context.WithValue(r.Context(), "isAdmin", isAdmin))
}

func TestServerRoles_CreateModeGate(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		isAdmin  bool
		wantCode int
		wantCall bool
	}{
		{"advanced non-admin allowed", "advanced", false, http.StatusOK, true},
		{"simple non-admin blocked", "simple", false, http.StatusForbidden, false},
		{"off non-admin blocked", "off", false, http.StatusForbidden, false},
		{"simple admin bypass", "simple", true, http.StatusOK, true},
		{"off admin bypass", "off", true, http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &serverRolesFakeStore{createID: 5, mode: tc.mode}
			h := NewServerRolesHandler(&AppState{Store: fs})
			rec := httptest.NewRecorder()
			h.CreateServerRole(rec, serverRoleReqAdmin("POST", "/api/server-roles", nil, tc.isAdmin,
				map[string]interface{}{"name": "R", "capabilities": []string{"files.read"}}))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if (fs.createCalls == 1) != tc.wantCall {
				t.Fatalf("createCalls = %d, wantCall %v", fs.createCalls, tc.wantCall)
			}
		})
	}
}

// Role definitions are audited: a role's capabilities are every holder's, so
// changing one changes access for all of them. A delete of a role in another
// realm must not copy that role's name into this owner's audit row.
func TestServerRoles_CrudIsAudited(t *testing.T) {
	fs := &serverRolesFakeStore{mode: "advanced", createID: 9,
		getRole: &store.ServerRole{ID: 3, OwnerUserID: testActorID, Name: "Old", Capabilities: []string{"files.read"}}}
	h := NewServerRolesHandler(&AppState{Store: fs})
	body := map[string]interface{}{"name": "Builders", "capabilities": []string{"files.read"}}
	h.CreateServerRole(httptest.NewRecorder(), serverRoleReq("POST", "/api/server-roles", nil, body))
	h.UpdateServerRole(httptest.NewRecorder(), serverRoleReq("PATCH", "/api/server-roles/3", map[string]string{"id": "3"}, body))
	h.DeleteServerRole(httptest.NewRecorder(), serverRoleReq("DELETE", "/api/server-roles/3", map[string]string{"id": "3"}, nil))
	fs.getRole = &store.ServerRole{ID: 4, OwnerUserID: "someone-else", Name: "Theirs"}
	h.DeleteServerRole(httptest.NewRecorder(), serverRoleReq("DELETE", "/api/server-roles/4", map[string]string{"id": "4"}, nil))

	want := []string{AuditEventServerRoleCreated, AuditEventServerRoleUpdated, AuditEventServerRoleDeleted, AuditEventServerRoleDeleted}
	if len(fs.audits) != len(want) {
		t.Fatalf("got %d audit rows, want %d", len(fs.audits), len(want))
	}
	for i, ev := range fs.audits {
		if ev.EventType != want[i] || ev.ActorUserID == nil || *ev.ActorUserID != testActorID {
			t.Errorf("row %d = %s by %v, want %s by %s", i, ev.EventType, ev.ActorUserID, want[i], testActorID)
		}
	}
	if fs.audits[1].Metadata["previous_name"] != "Old" || fs.audits[2].Metadata["name"] != "Old" {
		t.Errorf("update/delete rows lost the previous definition: %v / %v", fs.audits[1].Metadata, fs.audits[2].Metadata)
	}
	if _, leaked := fs.audits[3].Metadata["name"]; leaked {
		t.Errorf("another realm's role name reached this owner's audit row: %v", fs.audits[3].Metadata)
	}
}
