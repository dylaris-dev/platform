package users

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/models"
)

// Rows of TestStaffCannotTakeOverAStrongerAccount that exercise this package.
// TestStaffCannotTakeOverAStrongerAccount drives every route. The refused case
// must answer 403 and change nothing; the member twin must get past the guard
// (it may then stop at a dependency this fake does not provide, but never at
// the guard).
func TestStaffCannotTakeOverAStrongerAccountViaUserRoutes(t *testing.T) {
	routes := []struct {
		name string
		call func(st *handlers.AppState, r *http.Request, w http.ResponseWriter)
		body string
	}{
		{"reset password", func(st *handlers.AppState, r *http.Request, w http.ResponseWriter) {
			NewUserHandler(st).ResetUserPassword(w, r)
		}, `{"password":"a-long-enough-password"}`},
		{"rename", func(st *handlers.AppState, r *http.Request, w http.ResponseWriter) {
			NewUsernameHistoryHandler(st).AdminRename(w, r)
		}, `{"username":"renamed"}`},
		{"change role", func(st *handlers.AppState, r *http.Request, w http.ResponseWriter) {
			NewUserHandler(st).SetUserRoleHandler(w, r)
		}, `{"role":"user"}`},
		{"change permission flags", func(st *handlers.AppState, r *http.Request, w http.ResponseWriter) {
			NewUserHandler(st).SetUserPermissionsHandler(w, r)
		}, `{}`},
		{"delete", func(st *handlers.AppState, r *http.Request, w http.ResponseWriter) {
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

// Rows of TestAdminAccountActionsRefuseWithoutTheirPassword that exercise this package.
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
func TestAdminUserActionsRefuseWithoutTheirPassword(t *testing.T) {
	routes := []struct {
		name string
		call func(st *handlers.AppState, r *http.Request, w http.ResponseWriter)
		body string
	}{
		{"reset password", func(st *handlers.AppState, r *http.Request, w http.ResponseWriter) {
			NewUserHandler(st).ResetUserPassword(w, r)
		}, `{"password":"a-long-enough-password"}`},
		{"change role", func(st *handlers.AppState, r *http.Request, w http.ResponseWriter) {
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
		NewUserHandler(&handlers.AppState{Store: fake}).CreateUser(rec, r)
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
		// An equal: holding what the target holds is what lets these saves
		// reach the unchanged-value check at all (TestTheLegacyRightsGuardTheAccount).
		fs.users[guardStaff].Role = "support"
		fs.users[guardStaff].CanChangeResources = true
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
		st := &handlers.AppState{Store: &faultyRoleStore{guardFakeStore: fs, failFor: guardStronger}, Authz: authz.NewResolver(fs)}
		target, _ := fs.GetUserByID(guardStronger)
		if handlers.MayManageAccount(st, guardRequest("GET", guardStaff, false, guardStronger, ""), target) {
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
		NewUserHandler(&handlers.AppState{Store: fake}).CreateUser(rec, r.WithContext(context.WithValue(r.Context(), "isAdmin", true)))
		if rec.Code != http.StatusConflict || fake.created != nil {
			t.Fatalf("status = %d, created = %v", rec.Code, fake.created)
		}
	})
}

// mayManageAccount passes on yourself. Through the admin routes that let a
// staff member put themselves in any support team - which decides the tickets
// they see - and rename themselves past the profile's cooldown.
func TestStaffCannotSetTheirOwnTeamOrRenameThemselves(t *testing.T) {
	fs := newGuardFakeStore()
	fs.users[guardStaff].Password = testReauthHash // a valid re-proof, so only the self rule can refuse
	h := NewUserHandler(guardState(fs))
	rec := httptest.NewRecorder()
	h.SetUserPermissionsHandler(rec, guardRequest("PUT", guardStaff, false, guardStaff,
		`{"canDeleteServers":false,"canChangeResources":false,"supportTeam":"billing","reauth":{"password":"`+testReauthPassword+`"}}`))
	if rec.Code != http.StatusForbidden || len(fs.touched) != 0 {
		t.Fatalf("own permissions: status %d, touched %v; want 403 and nothing written", rec.Code, fs.touched)
	}

	rh := &UsernameHistoryHandler{state: guardState(fs)}
	rec = httptest.NewRecorder()
	rh.AdminRename(rec, guardRequest("PATCH", guardStaff, false, guardStaff, `{"username":"staff2"}`))
	if rec.Code != http.StatusForbidden || len(fs.touched) != 0 {
		t.Fatalf("own rename: status %d, touched %v; want 403 and nothing written", rec.Code, fs.touched)
	}
}

// The guard reads the target's CURRENT team; the save names the next one.
// Staff took over a manageable colleague and moved them into a team whose
// tickets the cross-team setting withholds from the caller.
func TestStaffPlaceOthersOnlyInTheirOwnTeam(t *testing.T) {
	for _, c := range []struct {
		team    string
		refused bool
	}{{"b", true}, {"a", false}, {"", false}} {
		fs := newGuardFakeStore()
		fs.users[guardStaff].SupportTeam = "a"
		rec := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }() // past the guard, the fake has no flag writer
			NewUserHandler(guardState(fs)).SetUserPermissionsHandler(rec,
				guardRequest("PUT", guardStaff, false, guardMember, `{"supportTeam":"`+c.team+`"}`))
		}()
		got := strings.Contains(rec.Body.String(), "your own support team")
		if got != c.refused {
			t.Errorf("team %q: refused = %v, want %v (%d %s)", c.team, got, c.refused, rec.Code, rec.Body)
		}
	}
}

// An operator sets someone's password because the account is no longer in its
// owner's hands. The sessions ended with it; the API keys kept working.
func TestAnAdminPasswordResetRevokesTheAccountsAPIKeys(t *testing.T) {
	base := newGuardFakeStore()
	base.users[guardAdmin].Password = testReauthHash
	fs := &keyedGuardStore{guardFakeStore: base, keys: []models.APIKey{{ID: 7}, {ID: 8}}}
	h := NewUserHandler(&handlers.AppState{Store: fs, Authz: authz.NewResolver(fs)})
	rec := httptest.NewRecorder()
	h.ResetUserPassword(rec, guardRequest("PUT", guardAdmin, true, guardMember,
		`{"password":"a-brand-new-password","reauth":{"password":"`+testReauthPassword+`"}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: status %d: %s", rec.Code, rec.Body.String())
	}
	if len(fs.revoked) != 2 {
		t.Fatalf("revoked keys %v, want both of the account's keys", fs.revoked)
	}
}

type emailTakenCreateStore struct{ createUserFakeStore }

func (f *emailTakenCreateStore) GetUserByEmail(string) (*models.User, error) {
	return &models.User{ID: "someone"}, nil
}
