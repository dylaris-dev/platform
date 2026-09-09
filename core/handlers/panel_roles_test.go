package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/store"
)

// panelRolesFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the panel-role methods are overridden. Any
// other call would panic - these tests never make one.
type panelRolesFakeStore struct {
	store.Store
	roles       []store.PanelRole
	getRole     *store.PanelRole
	getRoleErr  error
	createID    int
	createErr   error
	createCalls int
	updateCalls int
	deleteCalls int
	assignments []store.PanelAssignment
}

func (f *panelRolesFakeStore) ListPanelRoles() ([]store.PanelRole, error) { return f.roles, nil }
func (f *panelRolesFakeStore) GetPanelRole(int) (*store.PanelRole, error) {
	return f.getRole, f.getRoleErr
}
func (f *panelRolesFakeStore) CreatePanelRole(name string, caps []string, createdBy *string) (int, error) {
	f.createCalls++
	return f.createID, f.createErr
}
func (f *panelRolesFakeStore) UpdatePanelRole(id int, name string, caps []string) error {
	f.updateCalls++
	return nil
}
func (f *panelRolesFakeStore) DeletePanelRole(int) error {
	f.deleteCalls++
	return nil
}
func (f *panelRolesFakeStore) ListUserPanelAssignments() ([]store.PanelAssignment, error) {
	return f.assignments, nil
}

func panelRoleReq(method, target string, vars map[string]string, isAdmin bool, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, target, &buf)
	if vars != nil {
		r = mux.SetURLVars(r, vars)
	}
	ctx := context.WithValue(r.Context(), "isAdmin", isAdmin)
	ctx = context.WithValue(ctx, "userID", "11111111-1111-1111-1111-111111111111")
	return r.WithContext(ctx)
}

// Phase 4 Task 12: panelroles.write is now enforced at the route chokepoint
// (RequireCap wrapping in routes.go), not in-handler, so the old pure
// non-admin-forbidden case moved to routes_authz_test.go
// (TestCap_PanelRolesReadVsWriteVsDelete) where it runs through the real
// resolver. What remains here is the handler's own, still-live validation
// (unknown/non-panel cap rejection, system-role protection).

func TestPanelRoles_CreateRejectsUnknownCap(t *testing.T) {
	fs := &panelRolesFakeStore{}
	h := NewPanelRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.CreatePanelRole(rec, panelRoleReq("POST", "/api/admin/panel-roles", nil, true,
		map[string]interface{}{"name": "x", "capabilities": []string{"does.not.exist"}}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if fs.createCalls != 0 {
		t.Fatalf("expected no CreatePanelRole call, got %d", fs.createCalls)
	}
}

func TestPanelRoles_CreateRejectsNonPanelCap(t *testing.T) {
	// files.read is a SERVER-scope cap; a PANEL role must reject it.
	fs := &panelRolesFakeStore{}
	h := NewPanelRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.CreatePanelRole(rec, panelRoleReq("POST", "/api/admin/panel-roles", nil, true,
		map[string]interface{}{"name": "x", "capabilities": []string{"files.read"}}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if fs.createCalls != 0 {
		t.Fatalf("expected no CreatePanelRole call, got %d", fs.createCalls)
	}
}

func TestPanelRoles_CreateHappyPath(t *testing.T) {
	fs := &panelRolesFakeStore{createID: 5}
	h := NewPanelRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.CreatePanelRole(rec, panelRoleReq("POST", "/api/admin/panel-roles", nil, true,
		map[string]interface{}{"name": "Support L1", "capabilities": []string{"tickets.read", "users.read"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if fs.createCalls != 1 {
		t.Fatalf("expected 1 CreatePanelRole call, got %d", fs.createCalls)
	}
	var resp struct {
		Success bool `json:"success"`
		Role    struct {
			ID           int      `json:"id"`
			Capabilities []string `json:"capabilities"`
			IsSystem     bool     `json:"isSystem"`
		} `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || resp.Role.ID != 5 || len(resp.Role.Capabilities) != 2 || resp.Role.IsSystem {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestPanelRoles_UpdateRejectsSystemRole(t *testing.T) {
	fs := &panelRolesFakeStore{getRole: &store.PanelRole{ID: 1, Name: "admin", IsSystem: true}}
	h := NewPanelRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.UpdatePanelRole(rec, panelRoleReq("PATCH", "/api/admin/panel-roles/1", map[string]string{"id": "1"}, true,
		map[string]interface{}{"name": "admin2", "capabilities": []string{"users.read"}}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if fs.updateCalls != 0 {
		t.Fatalf("expected no UpdatePanelRole call, got %d", fs.updateCalls)
	}
}

func TestPanelRoles_UpdateHappyPath(t *testing.T) {
	fs := &panelRolesFakeStore{getRole: &store.PanelRole{ID: 2, Name: "custom", IsSystem: false}}
	h := NewPanelRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.UpdatePanelRole(rec, panelRoleReq("PATCH", "/api/admin/panel-roles/2", map[string]string{"id": "2"}, true,
		map[string]interface{}{"name": "custom2", "capabilities": []string{"users.read", "audit.read"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if fs.updateCalls != 1 {
		t.Fatalf("expected 1 UpdatePanelRole call, got %d", fs.updateCalls)
	}
}

func TestPanelRoles_DeleteRejectsSystemRole(t *testing.T) {
	fs := &panelRolesFakeStore{getRole: &store.PanelRole{ID: 1, Name: "admin", IsSystem: true}}
	h := NewPanelRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.DeletePanelRole(rec, panelRoleReq("DELETE", "/api/admin/panel-roles/1", map[string]string{"id": "1"}, true, nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if fs.deleteCalls != 0 {
		t.Fatalf("expected no DeletePanelRole call, got %d", fs.deleteCalls)
	}
}

func TestPanelRoles_DeleteHappyPath(t *testing.T) {
	fs := &panelRolesFakeStore{getRole: &store.PanelRole{ID: 2, Name: "custom", IsSystem: false}}
	h := NewPanelRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.DeletePanelRole(rec, panelRoleReq("DELETE", "/api/admin/panel-roles/2", map[string]string{"id": "2"}, true, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if fs.deleteCalls != 1 {
		t.Fatalf("expected 1 DeletePanelRole call, got %d", fs.deleteCalls)
	}
}

func TestPanelRoles_ListReturnsRoles(t *testing.T) {
	fs := &panelRolesFakeStore{roles: []store.PanelRole{
		{ID: 1, Name: "admin", Capabilities: []string{"users.read"}, IsSystem: true},
	}}
	h := NewPanelRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.ListPanelRoles(rec, panelRoleReq("GET", "/api/admin/panel-roles", nil, true, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Roles   []struct {
			Name     string `json:"name"`
			IsSystem bool   `json:"isSystem"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || len(resp.Roles) != 1 || resp.Roles[0].Name != "admin" || !resp.Roles[0].IsSystem {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// The assignments list is the Roles screen's whole picture of who holds what.
// Two things it must not do: turn an empty override set into a JSON null, and
// turn "no role" into a role id. The panel renders a badge off one and a count
// off the other, so either would show a privilege nobody has.
func TestPanelRoles_ListAssignments(t *testing.T) {
	roleID := 3
	fs := &panelRolesFakeStore{assignments: []store.PanelAssignment{
		{UserID: "u1", PanelRoleID: &roleID},
		{UserID: "u2", CapOverrides: store.CapOverrides{Grant: []string{"nodes.read"}}},
	}}
	h := NewPanelRolesHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.ListPanelAssignments(rec, panelRoleReq("GET", "/api/admin/panel-roles/assignments", nil, true, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Decoded from the raw body rather than through a struct, so an omitted or
	// null field is visible instead of arriving as a zero value.
	var raw struct {
		Success     bool `json:"success"`
		Assignments []struct {
			UserID      string    `json:"userId"`
			PanelRoleID *int      `json:"panelRoleId"`
			GrantCaps   *[]string `json:"grantCaps"`
			DenyCaps    *[]string `json:"denyCaps"`
		} `json:"assignments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !raw.Success || len(raw.Assignments) != 2 {
		t.Fatalf("got %+v", raw)
	}

	first := raw.Assignments[0]
	if first.PanelRoleID == nil || *first.PanelRoleID != 3 {
		t.Errorf("panelRoleId = %v, want 3", first.PanelRoleID)
	}
	if first.GrantCaps == nil || first.DenyCaps == nil {
		t.Errorf("empty caps encoded as null, want []: %s", rec.Body.String())
	}

	second := raw.Assignments[1]
	if second.PanelRoleID != nil {
		t.Errorf("panelRoleId = %v, want null for an overrides-only row", *second.PanelRoleID)
	}
	if second.GrantCaps == nil || len(*second.GrantCaps) != 1 {
		t.Errorf("grantCaps = %v, want the one that was stored", second.GrantCaps)
	}
}

// No assignments must encode as [], not null: the panel iterates it.
func TestPanelRoles_ListAssignmentsEmptyIsAList(t *testing.T) {
	h := NewPanelRolesHandler(&AppState{Store: &panelRolesFakeStore{}})
	rec := httptest.NewRecorder()
	h.ListPanelAssignments(rec, panelRoleReq("GET", "/api/admin/panel-roles/assignments", nil, true, nil))

	var raw struct {
		Assignments *[]struct{} `json:"assignments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if raw.Assignments == nil {
		t.Fatalf("assignments encoded as null, want []: %s", rec.Body.String())
	}
}
