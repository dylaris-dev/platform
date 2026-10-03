package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// What a STAFF member - a panel capability, not admin - may no longer do.
// Found by the round-38 audit; the platform has no staff today, so each of
// these is closed before the first one is hired.

const staffID = "33333333-3333-3333-3333-333333333333"

func staffRequest(method string, vars map[string]string, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	ctx := context.WithValue(r.Context(), "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", staffID)
	ctx = context.WithValue(ctx, "username", "staff")
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

// servers.write handed the owner short-circuit - files, console, RCON,
// backups - of any server on the platform's machines to whoever it named, the
// caller included, and recorded nothing.
func TestOnlyAnAdminChangesAServersOwner(t *testing.T) {
	rw := httptest.NewRecorder()
	(&ServerHandler{state: foreignState(visPlatformNode())}).AdminUpdateServerOwner(rw,
		staffRequest("PATCH", map[string]string{"id": "7"}, `{"userId":"`+staffID+`"}`))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("staff changed a server's owner: status %d", rw.Code)
	}
}

// foreignToCaller is false for a node the CALLER owns, so a move onto the
// caller's own machine passed it: root on the box now holding a customer's
// world.
func TestAServerIsNotMovedOntoTheCallersOwnMachine(t *testing.T) {
	caller := visAdmin
	for _, c := range []struct {
		name   string
		target *models.Node
		refuse bool
	}{
		{"the caller's own machine", &models.Node{ID: 8, OwnerID: &caller}, true},
		{"a platform machine", &models.Node{ID: 8, Status: "offline"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &twoNodeFakeStore{foreignFakeStore: foreignFakeStore{node: &models.Node{ID: 7}, byon: "true"}, target: c.target}
			s := &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Migration: &services.MigrationOrchestrator{}}
			rw := httptest.NewRecorder()
			(&ServerHandler{state: s}).MoveServer(rw, foreignAdminRequest("POST", map[string]string{"id": "3"}, `{"targetNodeId":8}`))
			if (rw.Code == http.StatusForbidden) != c.refuse {
				t.Fatalf("status %d, refuse=%v (%s)", rw.Code, c.refuse, rw.Body.String())
			}
		})
	}
}

// tickets.write is the support role, and this screen turns off the team
// isolation support is held in and shortens the audit trail that watches it.
func TestTheTicketPolicyIsTheOperators(t *testing.T) {
	rw := httptest.NewRecorder()
	(&TicketSettingsHandler{state: &AppState{}}).SaveSettings(rw,
		staffRequest("PUT", nil, `{"crossTeamVisibility":true,"auditRetentionDays":1}`))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("staff changed the ticket policy: status %d", rw.Code)
	}
}

type demoStaffStore struct {
	store.Store
	user  *models.User
	roleC []string
}

func (f *demoStaffStore) GetUserByUsername(string) (*models.User, error) { return f.user, nil }
func (f *demoStaffStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	if f.roleC == nil {
		return nil, store.CapOverrides{}, nil
	}
	id := 1
	return &id, store.CapOverrides{}, nil
}
func (f *demoStaffStore) GetPanelRole(int) (*store.PanelRole, error) {
	return &store.PanelRole{Capabilities: f.roleC}, nil
}
func (f *demoStaffStore) SetSetting(string, string) error { return nil }

// Anyone can open a demo session, and it reads what the account reads.
func TestAStaffAccountCannotBeTheDemo(t *testing.T) {
	for _, c := range []struct {
		name   string
		caps   []string
		refuse bool
	}{
		{"a support account", []string{"users.read", "audit.read"}, true},
		{"an ordinary account", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &demoStaffStore{user: &models.User{ID: "u-demo", Username: "demo"}, roleC: c.caps}
			rw := httptest.NewRecorder()
			(&ServerHandler{state: &AppState{Store: st, StoreEnabled: true}}).SetDemoAccount(rw,
				foreignAdminRequest("PUT", nil, `{"username":"demo"}`))
			if (rw.Code == http.StatusBadRequest) != c.refuse {
				t.Fatalf("status %d, refuse=%v (%s)", rw.Code, c.refuse, rw.Body.String())
			}
		})
	}
}

type termsStore struct {
	store.Store
	target *models.User
}

func (f *termsStore) GetUserByID(string) (*models.User, error) { return f.target, nil }
func (f *termsStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{Grant: []string{"plans.write"}}, nil
}
func (f *termsStore) GetPanelRole(int) (*store.PanelRole, error) { return nil, nil }

// plans.write is not an admin capability. Its holder granted themselves BYON
// and unlimited limits, or lifted their own suspension, on four routes where
// only the billing status asked whose account it was.
func TestPlansWriteCannotBeSpentOnTheCallersOwnAccount(t *testing.T) {
	calls := map[string]func(s *AppState, w http.ResponseWriter, r *http.Request){
		"entitlement grant": func(s *AppState, w http.ResponseWriter, r *http.Request) { (&EntitlementHandler{state: s}).Grant(w, r) },
		"entitlement revoke": func(s *AppState, w http.ResponseWriter, r *http.Request) {
			(&EntitlementHandler{state: s}).Revoke(w, r)
		},
		"limit overrides": func(s *AppState, w http.ResponseWriter, r *http.Request) {
			(&PlansHandler{state: s}).SetUserLimitOverrides(w, r)
		},
		"billing overrides": func(s *AppState, w http.ResponseWriter, r *http.Request) {
			(&BillingHandler{state: s}).SetBillingOverrides(w, r)
		},
	}
	for name, call := range calls {
		t.Run(name+"/own account", func(t *testing.T) {
			st := &termsStore{target: &models.User{ID: staffID}}
			s := &AppState{Store: st, Authz: authz.NewResolver(st)}
			rw := httptest.NewRecorder()
			call(s, rw, staffRequest("POST", map[string]string{"id": staffID}, `{"kind":"byon","days":30}`))
			if rw.Code != http.StatusForbidden {
				t.Fatalf("status %d on the caller's own account", rw.Code)
			}
		})
		t.Run(name+"/an admin", func(t *testing.T) {
			st := &termsStore{target: &models.User{ID: visAdmin, IsAdmin: true}}
			s := &AppState{Store: st, Authz: authz.NewResolver(st)}
			rw := httptest.NewRecorder()
			call(s, rw, staffRequest("POST", map[string]string{"id": visAdmin}, `{"kind":"byon","days":30}`))
			if rw.Code != http.StatusForbidden {
				t.Fatalf("status %d on an admin's account", rw.Code)
			}
		})
	}
}

// TransferServer asked only where the CALLER may place, and an admin may place
// on a node of their own: the same move MoveServer refuses, through the other door.
func TestATransferIsNotOntoTheCallersOwnMachine(t *testing.T) {
	caller := visAdmin
	st := &twoNodeFakeStore{foreignFakeStore: foreignFakeStore{node: &models.Node{ID: 7}, byon: "true"},
		target: &models.Node{ID: 8, OwnerID: &caller, Status: "offline"}}
	s := &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Migration: &services.MigrationOrchestrator{}}
	rw := httptest.NewRecorder()
	(&ServerHandler{state: s}).TransferServer(rw, foreignAdminRequest("POST", map[string]string{"id": "3"}, `{"targetNodeId":8}`))
	if rw.Code != http.StatusForbidden || !bytes.Contains(rw.Body.Bytes(), []byte("platform's nodes")) {
		t.Fatalf("status %d: %s", rw.Code, rw.Body.String())
	}
}

// Without ownership in force an owner_id means nothing, as for every other
// ownership rule, and the node is an ordinary target.
func TestTheMoveRuleWaitsForOwnershipToBeInForce(t *testing.T) {
	caller := visAdmin
	st := &twoNodeFakeStore{foreignFakeStore: foreignFakeStore{node: &models.Node{ID: 7}, byon: "false"},
		target: &models.Node{ID: 8, OwnerID: &caller, Status: "offline"}}
	s := &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Migration: &services.MigrationOrchestrator{}}
	rw := httptest.NewRecorder()
	(&ServerHandler{state: s}).MoveServer(rw, foreignAdminRequest("POST", map[string]string{"id": "3"}, `{"targetNodeId":8}`))
	if rw.Code == http.StatusForbidden {
		t.Fatalf("refused with ownership off: %s", rw.Body.String())
	}
}

type demoLoginStore struct {
	demoStaffStore
}

func (f *demoLoginStore) GetSetting(key string) (string, error) {
	if key == demoAccountUUIDSetting {
		return "u-demo", nil
	}
	return "", nil
}
func (f *demoLoginStore) GetUserByID(string) (*models.User, error) { return f.user, nil }

// The account can be given a staff role after it was designated, and every
// public demo session would then hold it.
func TestADemoLoginRefusesAnAccountThatGainedStaffRights(t *testing.T) {
	st := &demoLoginStore{demoStaffStore{user: &models.User{ID: "u-demo", Username: "demo"}, roleC: []string{"users.read"}}}
	rw := httptest.NewRecorder()
	(&AuthHandler{state: &AppState{Store: st, StoreEnabled: true}, jwtKey: []byte("k")}).DemoLogin(rw, httptest.NewRequest("POST", "/x", nil))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("a demo session was issued for a staff account: status %d", rw.Code)
	}
}
