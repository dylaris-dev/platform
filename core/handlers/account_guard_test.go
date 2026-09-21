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
func (f *guardFakeStore) GetUserRegionIDs(string) ([]string, error)     { return nil, nil }
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
		{"reset password", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			NewUserHandler(st).ResetUserPassword(w, r)
		}, `{"password":"a-long-enough-password"}`},
		{"reset 2FA", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			(&AuthHandler{state: st}).AdminResetTOTPHandler(w, r)
		}, ``},
		{"change email", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			NewUserEmailHandler(st).SetEmail(w, r)
		}, `{"email":"attacker@example.com"}`},
		{"rename", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			NewUsernameHistoryHandler(st).AdminRename(w, r)
		}, `{"username":"renamed"}`},
		{"change role", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			NewUserHandler(st).SetUserRoleHandler(w, r)
		}, `{"role":"user"}`},
		{"change permission flags", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			NewUserHandler(st).SetUserPermissionsHandler(w, r)
		}, `{}`},
		{"delete", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			NewUserHandler(st).DeleteUser(w, r)
		}, ``},
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

// Promoting to admin, and creating one, are an admin's alone - including
// promoting oneself, which the per-target check would allow.
func TestOnlyAnAdminMakesAnAdmin(t *testing.T) {
	t.Run("role admin, on themselves", func(t *testing.T) {
		fs := newGuardFakeStore()
		rec := httptest.NewRecorder()
		NewUserHandler(guardState(fs)).SetUserRoleHandler(rec, guardRequest("PUT", guardStaff, false, guardStaff, `{"role":"admin"}`))
		if rec.Code != http.StatusForbidden || len(fs.touched) != 0 {
			t.Fatalf("status = %d, touched = %v", rec.Code, fs.touched)
		}
	})
	t.Run("role support, on a member", func(t *testing.T) {
		fs := newGuardFakeStore()
		rec := httptest.NewRecorder()
		NewUserHandler(guardState(fs)).SetUserRoleHandler(rec, guardRequest("PUT", guardStaff, false, guardMember, `{"role":"support"}`))
		if rec.Code != http.StatusForbidden || len(fs.touched) != 0 {
			t.Fatalf("status = %d, touched = %v", rec.Code, fs.touched)
		}
	})
	t.Run("create an admin", func(t *testing.T) {
		fake := &createUserFakeStore{}
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/users", bytes.NewBufferString(`{"username":"sneaky","password":"correct-horse-battery","isAdmin":true}`))
		r = r.WithContext(context.WithValue(r.Context(), "isAdmin", false))
		NewUserHandler(&AppState{Store: fake}).CreateUser(rec, r)
		if rec.Code != http.StatusForbidden || fake.created != nil {
			t.Fatalf("status = %d, created = %v", rec.Code, fake.created)
		}
	})
	t.Run("grant resource changes one does not hold", func(t *testing.T) {
		fs := newGuardFakeStore()
		rec := httptest.NewRecorder()
		NewUserHandler(guardState(fs)).SetUserPermissionsHandler(rec, guardRequest("PUT", guardStaff, false, guardStaff, `{"canChangeResources":true}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
	})
}

// The second review's findings: two saves the panel makes on every edit, and
// the places that opened on a lookup error.
func TestTheAccountGuardAfterReview(t *testing.T) {
	const supportMember = "10000000-0000-4000-8000-000000000005"
	withSupport := func() *guardFakeStore {
		fs := newGuardFakeStore()
		fs.users[supportMember] = &models.User{ID: supportMember, Username: "support-member", Role: "support", CanChangeResources: true}
		return fs
	}

	t.Run("re-sending a support member's own role is not a promotion", func(t *testing.T) {
		fs := withSupport()
		rec := httptest.NewRecorder()
		NewUserHandler(guardState(fs)).SetUserRoleHandler(rec, guardRequest("PUT", guardStaff, false, supportMember, `{"role":"support"}`))
		if rec.Code == http.StatusForbidden {
			t.Fatalf("the panel's save of an unchanged role was refused: %s", rec.Body.String())
		}
	})
	t.Run("re-sending a resource flag the account already has is not a grant", func(t *testing.T) {
		fs := withSupport()
		rec := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }() // past the guard, the fake has no flag writer
			NewUserHandler(guardState(fs)).SetUserPermissionsHandler(rec, guardRequest("PUT", guardStaff, false, supportMember, `{"canChangeResources":true}`))
		}()
		if rec.Code == http.StatusForbidden {
			t.Fatalf("the panel's save of an unchanged flag was refused: %s", rec.Body.String())
		}
	})
	t.Run("a target whose role cannot be read is not powerless", func(t *testing.T) {
		fs := newGuardFakeStore()
		st := &AppState{Store: &faultyRoleStore{guardFakeStore: fs, failFor: guardStronger}, Authz: authz.NewResolver(fs)}
		target, _ := fs.GetUserByID(guardStronger)
		if mayManageAccount(st, guardRequest("GET", guardStaff, false, guardStronger, ""), target) {
			t.Error("a role lookup error made a stronger account manageable")
		}
	})
	t.Run("delete refuses when the account cannot be read", func(t *testing.T) {
		fs := newGuardFakeStore()
		rec := httptest.NewRecorder()
		NewUserHandler(guardState(fs)).DeleteUser(rec, guardRequest("DELETE", guardStaff, false, "10000000-0000-4000-8000-00000000ffff", ""))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("cancel an admin's deletion", func(t *testing.T) {
		fs := newGuardFakeStore()
		rec := httptest.NewRecorder()
		NewUserHandler(guardState(fs)).CancelUserDeletion(rec, guardRequest("POST", guardStaff, false, guardAdmin, ""))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
	})
	for _, target := range []string{guardStaff, guardAdmin} {
		t.Run("route limit on "+target, func(t *testing.T) {
			fs := newGuardFakeStore()
			rec := httptest.NewRecorder()
			NewUserHandler(guardState(fs)).SetUserRouteLimit(rec, guardRequest("PUT", guardStaff, false, target, `{"mode":"unlimited"}`))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
			}
		})
	}
	t.Run("create on an address already in use", func(t *testing.T) {
		fake := &emailTakenCreateStore{}
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/users", bytes.NewBufferString(`{"username":"puppet","password":"correct-horse-battery","email":"admin@example.com"}`))
		NewUserHandler(&AppState{Store: fake}).CreateUser(rec, r.WithContext(context.WithValue(r.Context(), "isAdmin", true)))
		if rec.Code != http.StatusConflict || fake.created != nil {
			t.Fatalf("status = %d, created = %v", rec.Code, fake.created)
		}
	})
}

type faultyRoleStore struct {
	*guardFakeStore
	failFor string
}

func (f *faultyRoleStore) GetUserPanelAuthz(id string) (*int, store.CapOverrides, error) {
	if id == f.failFor {
		return nil, store.CapOverrides{}, guardErr("connection reset by peer")
	}
	return f.guardFakeStore.GetUserPanelAuthz(id)
}

type emailTakenCreateStore struct{ createUserFakeStore }

func (f *emailTakenCreateStore) GetUserByEmail(string) (*models.User, error) {
	return &models.User{ID: "someone"}, nil
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
		{"reset password", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			NewUserHandler(st).ResetUserPassword(w, r)
		}, `{"password":"a-long-enough-password"}`},
		{"reset 2FA", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			(&AuthHandler{state: st}).AdminResetTOTPHandler(w, r)
		}, ``},
		{"change email", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			NewUserEmailHandler(st).SetEmail(w, r)
		}, `{"email":"attacker@example.com"}`},
		{"change role", func(st *AppState, r *http.Request, w http.ResponseWriter) {
			NewUserHandler(st).SetUserRoleHandler(w, r)
		}, `{"role":"support"}`},
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
