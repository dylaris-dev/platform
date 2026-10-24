package servers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
)

// The two remaining ownership checks that sat behind byonActive: an unreadable
// flag skipped them, so a settings fault let an admin place a server on a
// customer's machine by nodeId, or force-delete it with every server on it.
func TestPlacementAndForceDeleteFailClosed(t *testing.T) {
	fault := errors.New("connection refused")

	t.Run("create on a customer's machine", func(t *testing.T) {
		st := &foreignFakeStore{node: visOwnedNode(), settingErr: fault}
		h := &ServerHandler{state: &handlers.AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st)}}
		body := `{"uuid":"0f8c1a2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b","name":"x","nodeId":"7","docker":{"ram":1024,"cpuLimit":1,"diskLimit":10240}}`
		rec := httptest.NewRecorder()
		h.CreateServer(rec, foreignAdminRequest("POST", nil, body))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
		}
	})
}

// TestMovingAServerAcrossACustomersMachine: a move with either end on a
// customer's machine is refused; a move between two platform machines is not.
func TestMovingAServerAcrossACustomersMachine(t *testing.T) {
	owner := visOwner
	cases := []struct {
		name   string
		source *models.Node
		target *models.Node
		want   int // 404 refused; anything else got past the guard
	}{
		{"off a customer's machine", &models.Node{ID: 7, OwnerID: &owner}, &models.Node{ID: 8}, http.StatusNotFound},
		{"onto a customer's machine", &models.Node{ID: 7}, &models.Node{ID: 8, OwnerID: &owner}, http.StatusNotFound},
		{"between platform machines", &models.Node{ID: 7}, &models.Node{ID: 8, Status: "offline"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &twoNodeFakeStore{foreignFakeStore: foreignFakeStore{node: c.source, byon: "true"}, target: c.target}
			s := &handlers.AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Migration: &services.MigrationOrchestrator{}}
			rw := httptest.NewRecorder()
			(&ServerHandler{state: s}).MoveServer(rw, foreignAdminRequest("POST", map[string]string{"id": "3"}, `{"targetNodeId":8}`))
			if (c.want == http.StatusNotFound) != (rw.Code == http.StatusNotFound) {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, c.want, rw.Body.String())
			}
		})
	}
}

// TestATransferIsTheOwnersDecision: server.settings.write can be granted, and
// the target check only asks where the CALLER may place. So an invitee moved the
// owner's world onto the invitee's own machine, and an admin invited on a
// customer's machine pulled it onto ours. The owner's own transfer must pass.
func TestATransferIsTheOwnersDecision(t *testing.T) {
	owner := visOwner
	invitee := "33333333-3333-3333-3333-333333333333"
	inviteeNode := &models.Node{ID: 8, OwnerID: &invitee, Status: "offline"}
	ownerNode := &models.Node{ID: 8, OwnerID: &owner, Status: "offline"}
	cases := []struct {
		name    string
		user    string
		isAdmin bool
		source  *models.Node
		target  *models.Node
		refused bool
	}{
		{"an invitee onto their own machine", invitee, false, &models.Node{ID: 7}, inviteeNode, true},
		{"an admin off a customer's machine", visAdmin, true, &models.Node{ID: 7, OwnerID: &owner}, &models.Node{ID: 8, Status: "offline"}, true},
		{"the owner onto their own machine", visOwner, false, &models.Node{ID: 7}, ownerNode, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &twoNodeFakeStore{foreignFakeStore: foreignFakeStore{node: c.source, byon: "true"}, target: c.target}
			s := &handlers.AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Migration: &services.MigrationOrchestrator{}}
			r := httptest.NewRequest("POST", "/x", bytes.NewBufferString(`{"targetNodeId":8}`))
			ctx := context.WithValue(r.Context(), "isAdmin", c.isAdmin)
			ctx = context.WithValue(ctx, "userID", c.user)
			r = mux.SetURLVars(r.WithContext(ctx), map[string]string{"id": "3"})
			rw := httptest.NewRecorder()
			(&ServerHandler{state: s}).TransferServer(rw, r)
			if got := rw.Code == http.StatusForbidden && bytes.Contains(rw.Body.Bytes(), []byte("owner")); got != c.refused {
				t.Fatalf("refused = %v, want %v (status %d: %s)", got, c.refused, rw.Code, rw.Body.String())
			}
		})
	}
}

// Rows of TestWritesOnACustomersMachineAreNotFound that exercise this package.
// TestWritesOnACustomersMachineAreNotFound covers every route the review named.
// Past the guard, each platform twin stops at the next thing its fake lacks
// (a nil orchestrator, Redis, or node registry), and never at 404.
func TestServerWritesOnACustomersMachineAreNotFound(t *testing.T) {
	id := map[string]string{"id": "7"}
	routes := []struct {
		name string
		call func(t *testing.T, s *handlers.AppState, w http.ResponseWriter)
	}{
		{"reassign a server's owner", func(t *testing.T, s *handlers.AppState, w http.ResponseWriter) {
			(&ServerHandler{state: s}).AdminUpdateServerOwner(w, foreignAdminRequest("PATCH", id, `{"userId":"`+visAdmin+`"}`))
		}},
		{"flag a server as a demo", func(t *testing.T, s *handlers.AppState, w http.ResponseWriter) {
			s.StoreEnabled = true
			(&ServerHandler{state: s}).SetServerDemo(w, foreignAdminRequest("PATCH", id, `{"enabled":true}`))
		}},
		{"cancel a server's migration", func(t *testing.T, s *handlers.AppState, w http.ResponseWriter) {
			s.Redis, _ = newQuotaHTTPRedis(t) // asked before the server is loaded
			(&ServerHandler{state: s}).CancelMigration(w, foreignAdminRequest("POST", id, ""))
		}},
	}
	for _, rt := range routes {
		t.Run(rt.name+"/a customer's machine", func(t *testing.T) {
			rw := httptest.NewRecorder()
			rt.call(t, foreignState(visOwnedNode()), rw)
			if rw.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (%s)", rw.Code, rw.Body.String())
			}
		})
		t.Run(rt.name+"/a platform machine", func(t *testing.T) {
			rw := httptest.NewRecorder()
			func() {
				// A platform twin may run into a nil dependency the fake does not
				// provide; getting that far is the proof it passed the guard.
				defer func() {
					if p := recover(); p != nil && bytes.Contains(debug.Stack(), []byte("NodeOwnedByOther")) {
						t.Fatalf("the guard itself panicked on a platform machine: %v", p)
					}
				}()
				rt.call(t, foreignState(visPlatformNode()), rw)
			}()
			if rw.Code == http.StatusNotFound {
				t.Fatalf("a platform machine was refused: %s", rw.Body.String())
			}
		})
	}
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
			s := &handlers.AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Migration: &services.MigrationOrchestrator{}}
			rw := httptest.NewRecorder()
			(&ServerHandler{state: s}).MoveServer(rw, foreignAdminRequest("POST", map[string]string{"id": "3"}, `{"targetNodeId":8}`))
			if (rw.Code == http.StatusForbidden) != c.refuse {
				t.Fatalf("status %d, refuse=%v (%s)", rw.Code, c.refuse, rw.Body.String())
			}
		})
	}
}

// Without ownership in force an owner_id means nothing, as for every other
// ownership rule, and the node is an ordinary target.
func TestTheMoveRuleWaitsForOwnershipToBeInForce(t *testing.T) {
	caller := visAdmin
	st := &twoNodeFakeStore{foreignFakeStore: foreignFakeStore{node: &models.Node{ID: 7}, byon: "false"},
		target: &models.Node{ID: 8, OwnerID: &caller, Status: "offline"}}
	s := &handlers.AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Migration: &services.MigrationOrchestrator{}}
	rw := httptest.NewRecorder()
	(&ServerHandler{state: s}).MoveServer(rw, foreignAdminRequest("POST", map[string]string{"id": "3"}, `{"targetNodeId":8}`))
	if rw.Code == http.StatusForbidden {
		t.Fatalf("refused with ownership off: %s", rw.Body.String())
	}
}

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
			(&ServerHandler{state: &handlers.AppState{Store: st, StoreEnabled: true}}).SetDemoAccount(rw,
				foreignAdminRequest("PUT", nil, `{"username":"demo"}`))
			if (rw.Code == http.StatusBadRequest) != c.refuse {
				t.Fatalf("status %d, refuse=%v (%s)", rw.Code, c.refuse, rw.Body.String())
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
	s := &handlers.AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Migration: &services.MigrationOrchestrator{}}
	rw := httptest.NewRecorder()
	(&ServerHandler{state: s}).TransferServer(rw, foreignAdminRequest("POST", map[string]string{"id": "3"}, `{"targetNodeId":8}`))
	if rw.Code != http.StatusForbidden || !bytes.Contains(rw.Body.Bytes(), []byte("platform's nodes")) {
		t.Fatalf("status %d: %s", rw.Code, rw.Body.String())
	}
}

// A direct grant on a server linked to a proxy can be the owner narrowing what
// the member inherits from the proxy. Deleting it widens their access, so a
// member holding members.delete - on their own grant, or a co-manager's - may
// not; the owner may.
func TestRevokingANarrowGrantUnderAProxyIsTheOwnersCall(t *testing.T) {
	cases := []struct {
		name       string
		caller     string
		proxyGrant bool
		staleRealm bool
		wantDelete bool
	}{
		{"member, inherit grant on the proxy", "member", true, false, false},
		{"member, nothing inherited", "member", false, false, true},
		{"member, stale inherit grant the resolver ignores", "member", true, true, true},
		{"owner", "owner", true, false, true},
	}
	for _, c := range cases {
		t.Run("RemoveMember/"+c.name, func(t *testing.T) {
			fs := &revokeStore{proxyGrant: c.proxyGrant, staleRealm: c.staleRealm}
			h := handlers.NewMemberHandler(&handlers.AppState{Store: fs, Authz: authz.NewResolver(fs)})
			r := httptest.NewRequest(http.MethodDelete, "/api/servers/5/members/target", nil)
			r = mux.SetURLVars(r, map[string]string{"id": "5", "userId": "11111111-1111-1111-1111-111111111111"})
			r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "userID", c.caller), "isAdmin", false))
			h.RemoveMember(httptest.NewRecorder(), r)
			if fs.deleted != c.wantDelete {
				t.Errorf("deleted = %v, want %v", fs.deleted, c.wantDelete)
			}
		})
		t.Run("RevokeGrant/"+c.name, func(t *testing.T) {
			fs := &revokeStore{proxyGrant: c.proxyGrant, staleRealm: c.staleRealm}
			h := &ServerRolesHandler{state: &handlers.AppState{Store: fs, Authz: authz.NewResolver(fs)}}
			r := httptest.NewRequest(http.MethodDelete, "/api/grants", bytes.NewBufferString(`{"username":"t","serverId":5}`))
			r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "userID", c.caller), "isAdmin", false))
			// The member needs members.delete on 5 to get as far as the check.
			if c.caller == "member" {
				fs2 := &memberDeleteStore{revokeStore: fs}
				h.state = &handlers.AppState{Store: fs2, Authz: authz.NewResolver(fs2)}
			}
			h.RevokeGrant(httptest.NewRecorder(), r)
			if fs.deleted != c.wantDelete {
				t.Errorf("deleted = %v, want %v", fs.deleted, c.wantDelete)
			}
		})
	}
}

// Toggling goes through the same locked edit as the delete path, and must add,
// keep others, and remove - including a duplicate a racing toggle left behind.
func TestSetServerDemoEditsTheListInPlace(t *testing.T) {
	fs := &demoListStore{settings: map[string]string{handlers.DemoServerUUIDsSetting: `["other","srv-7","srv-7"]`}}
	s := &handlers.AppState{Store: fs, StoreEnabled: true}
	toggle := func(on bool) {
		body := `{"enabled":false}`
		if on {
			body = `{"enabled":true}`
		}
		w := httptest.NewRecorder()
		(&ServerHandler{state: s}).SetServerDemo(w, foreignAdminRequest("PATCH", map[string]string{"id": "7"}, body))
		if w.Code != http.StatusOK {
			t.Fatalf("SetServerDemo(%v) = %d %s", on, w.Code, w.Body)
		}
	}
	toggle(false)
	if got := fs.settings[handlers.DemoServerUUIDsSetting]; got != `["other"]` {
		t.Fatalf("after off: %s", got)
	}
	toggle(true)
	toggle(true)
	if got := fs.settings[handlers.DemoServerUUIDsSetting]; got != `["other","srv-7"]` {
		t.Fatalf("after on twice: %s", got)
	}
}

// A delete that lands between the handler's load and the locked edit must not
// see its dropDemoServers undone by the toggle writing the UUID back.
func TestSetServerDemoDoesNotReAddADeletedServer(t *testing.T) {
	fs := &demoListStore{settings: map[string]string{handlers.DemoServerUUIDsSetting: `["other"]`}, deleteInLock: true}
	s := &handlers.AppState{Store: fs, StoreEnabled: true}
	w := httptest.NewRecorder()
	(&ServerHandler{state: s}).SetServerDemo(w, foreignAdminRequest("PATCH", map[string]string{"id": "7"}, `{"enabled":true}`))
	if w.Code != http.StatusNotFound || fs.settings[handlers.DemoServerUUIDsSetting] != `["other"]` {
		t.Fatalf("got %d, list %s", w.Code, fs.settings[handlers.DemoServerUUIDsSetting])
	}
}
