package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/store"
)

// userRoleFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods SetUserRoleHandler and
// SetUserPermissionsHandler touch are overridden. Any other call would
// panic - these tests never make one.
type userRoleFakeStore struct {
	store.Store
	users      []models.User
	targetUser *models.User
	getUserErr error

	setRoleErr   error
	setRoleCalls []setRoleCall

	setPermsCalls []setPermsCall

	panelRole         *store.PanelRole
	panelRoleErr      error
	setPanelRoleCalls []setPanelRoleCall
	setOverridesCalls []store.CapOverrides

	panelAuthzRoleID *int
	panelAuthzOv     store.CapOverrides
	panelAuthzErr    error
}

type setPanelRoleCall struct {
	userID string
	roleID *int
}

type setRoleCall struct {
	userID string
	role   string
}

type setPermsCall struct {
	userID             string
	canDeleteServers   bool
	canChangeResources bool
	supportTeam        string
}

func (f *userRoleFakeStore) ListUsers() ([]models.User, error) { return f.users, nil }
func (f *userRoleFakeStore) GetUserByID(string) (*models.User, error) {
	return f.targetUser, f.getUserErr
}
func (f *userRoleFakeStore) SetUserRole(userID, role string) error {
	f.setRoleCalls = append(f.setRoleCalls, setRoleCall{userID, role})
	return f.setRoleErr
}
func (f *userRoleFakeStore) SetUserPermissionFlags(userID string, canDeleteServers, canChangeResources bool, supportTeam string) error {
	f.setPermsCalls = append(f.setPermsCalls, setPermsCall{userID, canDeleteServers, canChangeResources, supportTeam})
	return nil
}
func (f *userRoleFakeStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }
func (f *userRoleFakeStore) GetPanelRole(int) (*store.PanelRole, error) {
	return f.panelRole, f.panelRoleErr
}
func (f *userRoleFakeStore) SetUserPanelRole(userID string, roleID *int) error {
	f.setPanelRoleCalls = append(f.setPanelRoleCalls, setPanelRoleCall{userID, roleID})
	return nil
}
func (f *userRoleFakeStore) SetUserPanelCapOverrides(userID string, ov store.CapOverrides) error {
	f.setOverridesCalls = append(f.setOverridesCalls, ov)
	return nil
}
func (f *userRoleFakeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return f.panelAuthzRoleID, f.panelAuthzOv, f.panelAuthzErr
}

const testTargetID = "11111111-1111-1111-1111-111111111111"
const testOtherAdminID = "22222222-2222-2222-2222-222222222222"
const testOtherUserID = "33333333-3333-3333-3333-333333333333"

func setRoleReq(targetID, actorID string, isAdmin bool, body map[string]interface{}) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("PUT", "/api/admin/users/"+targetID+"/role", bytes.NewReader(b))
	r = mux.SetURLVars(r, map[string]string{"id": targetID})
	ctx := context.WithValue(r.Context(), "isAdmin", isAdmin)
	if actorID != "" {
		ctx = context.WithValue(ctx, "userID", actorID)
	}
	return r.WithContext(ctx)
}

// TestSetUserRoleHandler_SelfDemotionGuard covers the meaningful decision
// logic of SetUserRoleHandler: an admin may not demote themselves if it
// would leave zero admins, but every other combination (demoting someone
// else, or a no-op self-reassignment to admin) is unaffected by admin count.
func TestSetUserRoleHandler_SelfDemotionGuard(t *testing.T) {
	cases := []struct {
		name          string
		targetID      string
		users         []models.User
		reqRole       string
		wantStatus    int
		wantSetCalled bool
	}{
		{
			name:          "self-demote as the last admin is blocked",
			targetID:      testTargetID,
			users:         []models.User{{ID: testTargetID, IsAdmin: true}},
			reqRole:       "user",
			wantStatus:    http.StatusConflict,
			wantSetCalled: false,
		},
		{
			name:          "self-demote with another admin remaining is allowed",
			targetID:      testTargetID,
			users:         []models.User{{ID: testTargetID, IsAdmin: true}, {ID: testOtherAdminID, IsAdmin: true}},
			reqRole:       "user",
			wantStatus:    http.StatusOK,
			wantSetCalled: true,
		},
		{
			name:          "self-reassignment to admin is a no-op and skips the guard",
			targetID:      testTargetID,
			users:         []models.User{{ID: testTargetID, IsAdmin: true}},
			reqRole:       "admin",
			wantStatus:    http.StatusOK,
			wantSetCalled: true,
		},
		{
			name:          "demoting a different user skips the guard entirely",
			targetID:      testOtherUserID,
			users:         []models.User{{ID: testTargetID, IsAdmin: true}},
			reqRole:       "user",
			wantStatus:    http.StatusOK,
			wantSetCalled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &userRoleFakeStore{
				users:      tc.users,
				targetUser: &models.User{ID: tc.targetID, Role: "admin", Password: testReauthHash},
			}
			h := NewUserHandler(&AppState{Store: fs})
			rec := httptest.NewRecorder()

			h.SetUserRoleHandler(rec, setRoleReq(tc.targetID, testTargetID, true, withReauth(map[string]interface{}{"role": tc.reqRole})))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := len(fs.setRoleCalls) == 1; got != tc.wantSetCalled {
				t.Fatalf("setRoleCalls = %+v, wantCalled %v", fs.setRoleCalls, tc.wantSetCalled)
			}
		})
	}
}

// Phase 4 Task 12: users.write is now enforced at the route chokepoint
// (RequireCap wrapping in routes.go), not in-handler, so the old pure
// non-admin-forbidden case moved to routes_authz_test.go
// (TestCap_UsersPanelReadVsWrite) where it runs through the real resolver.
// What remains here is the handler's own, still-live business logic (the
// self-demotion guard, validation, not-found).

func TestSetUserRoleHandler_InvalidRole(t *testing.T) {
	fs := &userRoleFakeStore{targetUser: &models.User{ID: testTargetID}}
	h := NewUserHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.SetUserRoleHandler(rec, setRoleReq(testTargetID, testOtherUserID, true, map[string]interface{}{"role": "superadmin"}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if len(fs.setRoleCalls) != 0 {
		t.Fatalf("expected no SetUserRole call, got %+v", fs.setRoleCalls)
	}
}

func TestSetUserRoleHandler_TargetNotFound(t *testing.T) {
	fs := &userRoleFakeStore{getUserErr: errors.New("no rows")}
	h := NewUserHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.SetUserRoleHandler(rec, setRoleReq(testTargetID, testOtherUserID, true, map[string]interface{}{"role": "admin"}))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func setPermsReq(targetID string, isAdmin bool, body map[string]interface{}) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("PUT", "/api/admin/users/"+targetID+"/permissions", bytes.NewReader(b))
	r = mux.SetURLVars(r, map[string]string{"id": targetID})
	ctx := context.WithValue(r.Context(), "isAdmin", isAdmin)
	// The handler re-authenticates the ACTING administrator, so a request
	// without one is answered before it reaches what these cases are about.
	ctx = context.WithValue(ctx, "userID", testOtherAdminID)
	return r.WithContext(ctx)
}

// The old pure non-admin-forbidden case moved to routes_authz_test.go
// (TestCap_UsersPanelReadVsWrite): users.write is enforced at the route
// chokepoint now, not in-handler (Phase 4 Task 12).

// TestSetUserPermissionsHandler_Success is a thin passthrough (no branching
// beyond the admin gate); this pins the argument order/mapping so a future
// field-shuffle regresses loudly instead of silently swapping flags.
func TestSetUserPermissionsHandler_Success(t *testing.T) {
	fs := &userRoleFakeStore{targetUser: &models.User{ID: testTargetID, Password: testReauthHash}}
	h := NewUserHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	body := withReauth(map[string]interface{}{"canDeleteServers": true, "canChangeResources": true, "supportTeam": "billing"})

	h.SetUserPermissionsHandler(rec, setPermsReq(testTargetID, true, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.setPermsCalls) != 1 {
		t.Fatalf("expected 1 store call, got %d", len(fs.setPermsCalls))
	}
	call := fs.setPermsCalls[0]
	// canDeleteServers arrives TRUE and is stored FALSE: deleting follows the
	// role, and this fake's target is not an admin. Forced rather than
	// rejected, so a stale client or a direct API call cannot leave a row
	// claiming a right ComputeEffectivePermissions no longer grants.
	if call.userID != testTargetID || call.canDeleteServers || !call.canChangeResources || call.supportTeam != "billing" {
		t.Fatalf("call = %+v, want userID=%s canDeleteServers=false canChangeResources=true supportTeam=billing", call, testTargetID)
	}
}

func setPanelRoleReq(targetID, actorID string, isAdmin bool, body map[string]interface{}) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("PUT", "/api/admin/users/"+targetID+"/panel-role", bytes.NewReader(b))
	r = mux.SetURLVars(r, map[string]string{"id": targetID})
	ctx := context.WithValue(r.Context(), "isAdmin", isAdmin)
	if actorID != "" {
		ctx = context.WithValue(ctx, "userID", actorID)
	}
	return r.WithContext(ctx)
}

// The old pure non-admin-forbidden case moved to routes_authz_test.go
// (TestCap_SetUserPanelRoleNeedsPanelRolesWrite): panelroles.write is
// enforced at the route chokepoint now, not in-handler (Phase 4 Task 12).

func TestSetUserPanelRoleHandler_RejectsNonPanelOverride(t *testing.T) {
	// files.read is a SERVER-scope cap; a panel override must reject it.
	fs := &userRoleFakeStore{}
	h := NewUserHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.SetUserPanelRoleHandler(rec, setPanelRoleReq(testTargetID, testOtherUserID, true, map[string]interface{}{
		"grantCaps": []string{"files.read"},
	}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if len(fs.setPanelRoleCalls) != 0 {
		t.Fatalf("expected no store mutation, got %+v", fs.setPanelRoleCalls)
	}
}

func TestSetUserPanelRoleHandler_RoleNotFound(t *testing.T) {
	fs := &userRoleFakeStore{panelRoleErr: errors.New("no rows")}
	h := NewUserHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.SetUserPanelRoleHandler(rec, setPanelRoleReq(testTargetID, testOtherUserID, true, map[string]interface{}{
		"panelRoleId": 99,
	}))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if len(fs.setPanelRoleCalls) != 0 {
		t.Fatalf("expected no store mutation, got %+v", fs.setPanelRoleCalls)
	}
}

func TestSetUserPanelRoleHandler_Success(t *testing.T) {
	fs := &userRoleFakeStore{
		panelRole:  &store.PanelRole{ID: 3, Name: "support"},
		targetUser: &models.User{ID: testOtherUserID, Password: testReauthHash},
	}
	h := NewUserHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.SetUserPanelRoleHandler(rec, setPanelRoleReq(testTargetID, testOtherUserID, true, withReauth(map[string]interface{}{
		"panelRoleId": 3,
		"grantCaps":   []string{"nodes.read"},
		"denyCaps":    []string{"users.delete"},
	})))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.setPanelRoleCalls) != 1 {
		t.Fatalf("expected 1 SetUserPanelRole call, got %d", len(fs.setPanelRoleCalls))
	}
	call := fs.setPanelRoleCalls[0]
	if call.userID != testTargetID || call.roleID == nil || *call.roleID != 3 {
		t.Fatalf("call = %+v, want userID=%s roleID=3", call, testTargetID)
	}
	if len(fs.setOverridesCalls) != 1 {
		t.Fatalf("expected 1 SetUserPanelCapOverrides call, got %d", len(fs.setOverridesCalls))
	}
	ov := fs.setOverridesCalls[0]
	if len(ov.Grant) != 1 || ov.Grant[0] != "nodes.read" || len(ov.Deny) != 1 || ov.Deny[0] != "users.delete" {
		t.Fatalf("overrides = %+v, want grant=[nodes.read] deny=[users.delete]", ov)
	}
}

func TestGetUserPanelRoleHandler_Success(t *testing.T) {
	rid := 3
	fs := &userRoleFakeStore{
		panelAuthzRoleID: &rid,
		panelAuthzOv:     store.CapOverrides{Grant: []string{"nodes.read"}, Deny: []string{"users.delete"}},
	}
	h := NewUserHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/admin/users/"+testTargetID+"/panel-role", nil)
	r = mux.SetURLVars(r, map[string]string{"id": testTargetID})
	h.GetUserPanelRoleHandler(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success     bool     `json:"success"`
		PanelRoleID *int     `json:"panelRoleId"`
		GrantCaps   []string `json:"grantCaps"`
		DenyCaps    []string `json:"denyCaps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || resp.PanelRoleID == nil || *resp.PanelRoleID != 3 {
		t.Fatalf("panelRoleId = %+v, want 3", resp.PanelRoleID)
	}
	if len(resp.GrantCaps) != 1 || resp.GrantCaps[0] != "nodes.read" || len(resp.DenyCaps) != 1 || resp.DenyCaps[0] != "users.delete" {
		t.Fatalf("overrides = grant %v deny %v", resp.GrantCaps, resp.DenyCaps)
	}
}
