package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// A panel capability is fleet-wide, so users.write said nothing about WHOSE
// account. A custom staff role holding it could reset an admin's password, point
// an admin's email at its own mailbox, make itself an admin, or take over a
// staff account holding more than it (panelroles.write). Written from that staff
// member's side, each with a twin the guard must let through.

const (
	guardStaff    = "10000000-0000-4000-8000-000000000001" // holds users.write
	guardAdmin    = "10000000-0000-4000-8000-000000000002"
	guardMember   = "10000000-0000-4000-8000-000000000003" // no panel role
	guardStronger = "10000000-0000-4000-8000-000000000004" // users.write + panelroles.write
)

type guardFakeStore struct {
	store.Store
	users   map[string]*models.User
	roleOf  map[string]int
	roles   map[int]*store.PanelRole
	touched []string // what a handler changed, by user id
}

func newGuardFakeStore() *guardFakeStore {
	return &guardFakeStore{
		users: map[string]*models.User{
			guardStaff:    {ID: guardStaff, Username: "staff", Role: "user"},
			guardAdmin:    {ID: guardAdmin, Username: "admin", Role: "admin", IsAdmin: true},
			guardMember:   {ID: guardMember, Username: "member", Role: "user"},
			guardStronger: {ID: guardStronger, Username: "stronger", Role: "user"},
		},
		roleOf: map[string]int{guardStaff: 1, guardStronger: 2},
		roles: map[int]*store.PanelRole{
			1: {ID: 1, Capabilities: []string{"users.read", "users.write", "users.delete"}},
			2: {ID: 2, Capabilities: []string{"users.read", "users.write", "users.delete", "panelroles.write"}},
		},
	}
}

func (f *guardFakeStore) GetUserByID(id string) (*models.User, error) {
	if u, ok := f.users[id]; ok {
		c := *u
		return &c, nil
	}
	return nil, errNotFoundForGuard
}

func (f *guardFakeStore) GetUserPanelAuthz(id string) (*int, store.CapOverrides, error) {
	if rid, ok := f.roleOf[id]; ok {
		return &rid, store.CapOverrides{}, nil
	}
	return nil, store.CapOverrides{}, nil
}

func (f *guardFakeStore) GetPanelRole(id int) (*store.PanelRole, error) { return f.roles[id], nil }
func (f *guardFakeStore) GetSetting(string) (string, error)             { return "", nil }

func (f *guardFakeStore) UpdateUserPassword(id, _ string) error {
	f.touched = append(f.touched, id)
	return nil
}
func (f *guardFakeStore) DisableUserTOTP(id string) error {
	f.touched = append(f.touched, id)
	return nil
}
func (f *guardFakeStore) SetUserRole(id, _ string) error {
	f.touched = append(f.touched, id)
	return nil
}
func (f *guardFakeStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }

// The email route looks for a collision before it re-authenticates, so the
// fake has to answer. Nobody holds the address these cases try.
func (f *guardFakeStore) GetUserByEmail(string) (*models.User, error) { return nil, nil }

type guardErr string

func (e guardErr) Error() string { return string(e) }

const errNotFoundForGuard = guardErr("not found")

func guardState(fs *guardFakeStore) *AppState {
	return &AppState{Store: fs, Authz: authz.NewResolver(fs)}
}

func guardRequest(method, actor string, isAdmin bool, target, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	ctx := context.WithValue(r.Context(), "userID", actor)
	ctx = context.WithValue(ctx, "username", "actor")
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	return mux.SetURLVars(r.WithContext(ctx), map[string]string{"id": target})
}

func TestMayManageAccount(t *testing.T) {
	fs := newGuardFakeStore()
	st := guardState(fs)
	cases := []struct {
		name    string
		actor   string
		isAdmin bool
		target  string
		want    bool
	}{
		{"an admin, on an admin", guardAdmin, true, guardAdmin, true},
		{"staff, on an admin", guardStaff, false, guardAdmin, false},
		{"staff, on a member", guardStaff, false, guardMember, true},
		{"staff, on staff holding more", guardStaff, false, guardStronger, false},
		{"stronger staff, on weaker staff", guardStronger, false, guardStaff, true},
		{"staff, on themselves", guardStaff, false, guardStaff, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target, _ := fs.GetUserByID(c.target)
			if got := mayManageAccount(st, guardRequest("GET", c.actor, c.isAdmin, c.target, ""), target); got != c.want {
				t.Errorf("mayManageAccount = %v, want %v", got, c.want)
			}
		})
	}
}

// TestStaffCannotTakeOverAStrongerAccount drives every route. The refused case
// must answer 403 and change nothing; the member twin must get past the guard
// (it may then stop at a dependency this fake does not provide, but never at
// the guard).
func TestStaffCannotTakeOverAStrongerAccount(t *testing.T) {
	routes := []struct {
		name string
		call func(st *AppState, r *http.Request, w http.ResponseWriter)
		body string
	}{
		{"reset 2FA", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			(&AuthHandler{state: st}).AdminResetTOTPHandler(w, r)
		}, ``},
		{"change email", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			NewUserEmailHandler(st).SetEmail(w, r)
		}, `{"email":"attacker@example.com"}`},
	}
	for _, rt := range routes {
		for _, target := range []string{guardAdmin, guardStronger} {
			t.Run(rt.name+"/refused on "+target, func(t *testing.T) {
				fs := newGuardFakeStore()
				rec := httptest.NewRecorder()
				rt.call(guardState(fs), guardRequest("PUT", guardStaff, false, target, rt.body), rec)
				if rec.Code != http.StatusForbidden || len(fs.touched) != 0 {
					t.Fatalf("status = %d, touched = %v (%s)", rec.Code, fs.touched, rec.Body.String())
				}
			})
		}
		t.Run(rt.name+"/a member is still manageable", func(t *testing.T) {
			fs := newGuardFakeStore()
			rec := httptest.NewRecorder()
			func() {
				defer func() {
					if p := recover(); p != nil && bytes.Contains(debug.Stack(), []byte("mayManageAccount")) {
						t.Fatalf("the guard itself panicked: %v", p)
					}
				}()
				rt.call(guardState(fs), guardRequest("PUT", guardStaff, false, guardMember, rt.body), rec)
			}()
			if rec.Code == http.StatusForbidden {
				t.Fatalf("a member was refused: %s", rec.Body.String())
			}
		})
	}
}

// The same routes, one question further along: an administrator who may manage
// the account still has to prove they are the one asking.
//
// Measured on production: with an admin session alone and no password, any
// account's password could be set, its second factor stripped and its address
// changed. The session-kill that covers a password change reaches none of it -
// it ends the VICTIM's sessions, while the borrowed admin session is the thing
// doing the asking.
//
// Every case drives the route with no reauth block at all and expects 401 with
// nothing written. Delete and rename are deliberately absent: removing an
// account and renaming one hand nobody durable access.
func TestAdminAccountActionsRefuseWithoutTheirPassword(t *testing.T) {
	routes := []struct {
		name string
		call func(st *AppState, r *http.Request, w http.ResponseWriter)
		body string
	}{
		{"reset 2FA", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			(&AuthHandler{state: st}).AdminResetTOTPHandler(w, r)
		}, ``},
		{"change email", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			NewUserEmailHandler(st).SetEmail(w, r)
		}, `{"email":"attacker@example.com"}`},
	}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			fs := newGuardFakeStore()
			rec := httptest.NewRecorder()
			rt.call(guardState(fs), guardRequest("PUT", guardAdmin, true, guardMember, rt.body), rec)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
			}
			if len(fs.touched) != 0 {
				t.Fatalf("the account was changed anyway: %v", fs.touched)
			}
		})
	}
}

func (f *guardFakeStore) SetUserPermissionFlags(id string, _, _ bool, _ string) error {
	f.touched = append(f.touched, id)
	return nil
}
func (f *guardFakeStore) UsernameTaken(string, string) (bool, error) { return false, nil }
func (f *guardFakeStore) RenameUser(id, _, _ string) error {
	f.touched = append(f.touched, id)
	return nil
}

type keyedGuardStore struct {
	*guardFakeStore
	keys    []models.APIKey
	revoked []int
}

func (f *keyedGuardStore) ListAPIKeysByUser(string) ([]models.APIKey, error) { return f.keys, nil }
func (f *keyedGuardStore) RevokeAPIKey(id int, _ string) error {
	f.revoked = append(f.revoked, id)
	return nil
}
func (f *keyedGuardStore) DeleteAllSSHKeys(string) (int, error) { return 0, nil }

type epochGuardStore struct {
	*keyedGuardStore
	bumped []string
}

func (f *epochGuardStore) BumpSessionEpoch(id string) (int, error) {
	f.bumped = append(f.bumped, id)
	return len(f.bumped), nil
}

// A 2FA reset is asked for because someone else may be holding the account.
// Their session used to outlive it: only a password change ended sessions.
func TestAnAdmin2FAResetEndsTheAccountsSessions(t *testing.T) {
	base := newGuardFakeStore()
	base.users[guardAdmin].Password = testReauthHash
	fs := &epochGuardStore{keyedGuardStore: &keyedGuardStore{guardFakeStore: base}}
	h := &AuthHandler{state: &AppState{Store: fs, Authz: authz.NewResolver(fs)}}
	rec := httptest.NewRecorder()
	h.AdminResetTOTPHandler(rec, guardRequest("DELETE", guardAdmin, true, guardMember,
		`{"reauth":{"password":"`+testReauthPassword+`"}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: status %d: %s", rec.Code, rec.Body.String())
	}
	if len(fs.bumped) != 1 || fs.bumped[0] != guardMember {
		t.Fatalf("sessions ended for %v, want the member's", fs.bumped)
	}
}

// The per-user rights that are not panel caps. A staff member with users.write
// and none of them took over an account holding one and so held it too: role
// support reads every ticket, can_change_resources resizes servers, a support
// team decides whose tickets are visible.
func TestTheLegacyRightsGuardTheAccount(t *testing.T) {
	cases := []struct {
		name   string
		staff  models.User
		target models.User
		want   bool
		caps   []string // extra panel caps for the staff role
	}{
		{"plain target", models.User{}, models.User{}, true, nil},
		{"support target, staff without tickets", models.User{}, models.User{Role: "support"}, false, nil},
		{"support target, support staff", models.User{Role: "support"}, models.User{Role: "support"}, true, nil},
		{"support target, staff reading tickets only", models.User{}, models.User{Role: "support"}, false, []string{"tickets.read"}},
		{"support target, staff with both ticket caps", models.User{}, models.User{Role: "support"}, true, []string{"tickets.read", "tickets.write"}},
		{"resource target, staff without", models.User{}, models.User{CanChangeResources: true}, false, nil},
		{"resource target, staff with", models.User{CanChangeResources: true}, models.User{CanChangeResources: true}, true, nil},
		{"other team", models.User{SupportTeam: "a"}, models.User{SupportTeam: "b"}, false, nil},
		{"team target, staff without team", models.User{}, models.User{SupportTeam: "b"}, false, nil},
		{"same team", models.User{SupportTeam: "b"}, models.User{SupportTeam: "b"}, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newGuardFakeStore()
			fs.roles[1].Capabilities = append(fs.roles[1].Capabilities, c.caps...)
			staff := c.staff
			staff.ID, staff.Username = guardStaff, "staff"
			if staff.Role == "" {
				staff.Role = "user"
			}
			fs.users[guardStaff] = &staff
			target := c.target
			target.ID, target.Username = guardMember, "member"
			if target.Role == "" {
				target.Role = "user"
			}
			fs.users[guardMember] = &target
			if got := mayManageAccount(guardState(fs), guardRequest("PUT", guardStaff, false, guardMember, ""), &target); got != c.want {
				t.Fatalf("mayManageAccount = %v, want %v", got, c.want)
			}
		})
	}
}
