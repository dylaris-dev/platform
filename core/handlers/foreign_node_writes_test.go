package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// What staff and admins may still do to a customer's machine, written from the
// admin's side like node_visibility_test.go, each with a platform twin that must
// get PAST the guard. Found by the review of step 1 of
// roadmap/node-ownership-and-access.md (defects 8 to 11).

// foreignFakeStore answers the node, the server on it and the BYON flag. A
// settingErr or nodeErr is a database fault, which must count as foreign.
type foreignFakeStore struct {
	store.Store
	node       *models.Node
	nodeErr    error
	byon       string
	settingErr error
}

func (f *foreignFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.nodeErr != nil {
		return nil, f.nodeErr
	}
	return f.node, nil
}

func (f *foreignFakeStore) GetSetting(key string) (string, error) {
	if key == "feature_byon_enabled" {
		return f.byon, f.settingErr
	}
	return "", nil
}

func (f *foreignFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-1", OwnerID: visOwner, NodeID: f.node.ID}, nil
}

func (f *foreignFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	return &models.Server{ID: 3, UUID: uuid, OwnerID: visOwner, NodeID: f.node.ID}, nil
}

func (f *foreignFakeStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id}, nil
}

func (f *foreignFakeStore) GetPanelRole(int) (*store.PanelRole, error)   { return nil, nil }
func (f *foreignFakeStore) GetServerRole(int) (*store.ServerRole, error) { return nil, nil }
func (f *foreignFakeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}
func (f *foreignFakeStore) GetServerGrant(int, string) (*store.ServerGrant, error) {
	return nil, errors.New("no grant")
}
func (f *foreignFakeStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	return nil, errors.New("no grant")
}

func TestNodeOwnedByOther(t *testing.T) {
	owner := visOwner
	owned := &models.Node{ID: 7, OwnerID: &owner}
	platform := &models.Node{ID: 7}
	cases := []struct {
		name string
		st   *foreignFakeStore
		user string
		want bool
	}{
		{"a platform machine", &foreignFakeStore{node: platform, byon: "true"}, visAdmin, false},
		{"a customer's machine, BYON on", &foreignFakeStore{node: owned, byon: "true"}, visAdmin, true},
		{"its owner", &foreignFakeStore{node: owned, byon: "true"}, visOwner, false},
		{"a customer's machine, BYON off", &foreignFakeStore{node: owned, byon: "false"}, visAdmin, false},
		// Fail closed: these decide whether an admin opens a customer's world.
		{"the node cannot be read", &foreignFakeStore{nodeErr: errors.New("connection refused")}, visAdmin, true},
		{"the flag cannot be read", &foreignFakeStore{node: owned, settingErr: errors.New("connection refused")}, visAdmin, true},
		// Never saved is not a fault: it is the default, which is off.
		{"the flag was never saved", &foreignFakeStore{node: owned, settingErr: sql.ErrNoRows}, visAdmin, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (&AppState{Store: c.st, FeatureFlags: services.NewFeatureFlags(c.st)}).NodeOwnedByOther(7, c.user); got != c.want {
				t.Errorf("NodeOwnedByOther = %v, want %v", got, c.want)
			}
		})
	}
}

// foreignState builds the state the way main does: the resolver asks the same
// predicate the handlers ask.
func foreignState(node *models.Node) *AppState {
	st := &foreignFakeStore{node: node, byon: "true"}
	s := &AppState{Store: st, Authz: authz.NewResolver(st), FeatureFlags: services.NewFeatureFlags(st)}
	s.Authz.SetForeignNode(s.NodeOwnedByOther)
	return s
}

func foreignAdminRequest(method string, vars map[string]string, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	ctx := context.WithValue(r.Context(), "isAdmin", true)
	ctx = context.WithValue(ctx, "userID", visAdmin)
	ctx = context.WithValue(ctx, "username", "admin")
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

// TestWritesOnACustomersMachineAreNotFound covers every route the review named.
// Past the guard, each platform twin stops at the next thing its fake lacks
// (a nil orchestrator, Redis, or node registry), and never at 404.
func TestWritesOnACustomersMachineAreNotFound(t *testing.T) {
	id := map[string]string{"id": "7"}
	routes := []struct {
		name string
		call func(t *testing.T, s *AppState, w http.ResponseWriter)
	}{
		{"reassign a server's owner", func(t *testing.T, s *AppState, w http.ResponseWriter) {
			(&ServerHandler{state: s}).AdminUpdateServerOwner(w, foreignAdminRequest("PATCH", id, `{"userId":"`+visAdmin+`"}`))
		}},
		{"flag a server as a demo", func(t *testing.T, s *AppState, w http.ResponseWriter) {
			s.StoreEnabled = true
			(&ServerHandler{state: s}).SetServerDemo(w, foreignAdminRequest("PATCH", id, `{"enabled":true}`))
		}},
		{"cancel a server's migration", func(t *testing.T, s *AppState, w http.ResponseWriter) {
			s.Redis, _ = newQuotaHTTPRedis(t) // asked before the server is loaded
			(&ServerHandler{state: s}).CancelMigration(w, foreignAdminRequest("POST", id, ""))
		}},
		{"reset pairing", func(t *testing.T, s *AppState, w http.ResponseWriter) {
			(&NodeAdmissionHandler{state: s}).ResetPairing(w, foreignAdminRequest("POST", id, ""))
		}},
		{"roll the key", func(t *testing.T, s *AppState, w http.ResponseWriter) {
			(&NodeAdmissionHandler{state: s}).RollSecret(w, foreignAdminRequest("POST", id, ""))
		}},
		{"set the CPU pool", func(t *testing.T, s *AppState, w http.ResponseWriter) {
			(&NodeHandler{state: s}).UpdateNode(w, foreignAdminRequest("PUT", id, `{}`))
		}},
		{"set overcommit", func(t *testing.T, s *AppState, w http.ResponseWriter) {
			r := foreignAdminRequest("PUT", id, `{"cpuOvercommitRatio":1,"ramOvercommitRatio":1}`)
			r.URL.Path = "/api/nodes/7/placement"
			(&PlacementHandler{state: s}).SetNodePlacement(w, r)
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
			s := &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Migration: &services.MigrationOrchestrator{}}
			rw := httptest.NewRecorder()
			(&ServerHandler{state: s}).MoveServer(rw, foreignAdminRequest("POST", map[string]string{"id": "3"}, `{"targetNodeId":8}`))
			if (c.want == http.StatusNotFound) != (rw.Code == http.StatusNotFound) {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, c.want, rw.Body.String())
			}
		})
	}
}

type twoNodeFakeStore struct {
	foreignFakeStore
	target *models.Node
}

func (f *twoNodeFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.target != nil && id == f.target.ID {
		return f.target, nil
	}
	return f.node, nil
}

func (f *twoNodeFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-1", OwnerID: visOwner, NodeID: f.node.ID}, nil
}

// TestFilesAndBeamAskTheResolverForAnAdmin: both answered an admin before the
// resolver was consulted, which made the resolver fence decorative.
func TestFilesAndBeamAskTheResolverForAnAdmin(t *testing.T) {
	for _, c := range []struct {
		name    string
		node    *models.Node
		allowed bool
	}{
		{"a customer's machine", visOwnedNode(), false},
		{"a platform machine", visPlatformNode(), true},
	} {
		t.Run("files/"+c.name, func(t *testing.T) {
			h := &FileHandler{state: foreignState(c.node)}
			r := foreignAdminRequest("GET", nil, "")
			r.URL.RawQuery = "server_uuid=srv-1"
			uuid, err := h.getServerUUID(r, "files.write")
			if (err == nil && uuid == "srv-1") != c.allowed {
				t.Fatalf("err = %v, allowed = %v", err, c.allowed)
			}
		})
		t.Run("beam/"+c.name, func(t *testing.T) {
			h := &BeamHandler{state: foreignState(c.node)}
			ok, perms := h.canBeam(foreignAdminRequest("GET", nil, ""), 3)
			if ok != c.allowed || perms.Write != c.allowed {
				t.Fatalf("canBeam = %v %+v, want %v", ok, perms, c.allowed)
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
			s := &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Migration: &services.MigrationOrchestrator{}}
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

// The older fences (ownedByOther, canManageNode, canPlaceOnNode, disk tools)
// answer through ownershipInForce, which must fail closed like the resolver's
// half: the two used to disagree for as long as the settings table did not answer.
func TestTheOlderFencesFailClosedToo(t *testing.T) {
	st := &foreignFakeStore{node: visOwnedNode(), settingErr: errors.New("connection refused")}
	state := &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st)}
	r := foreignAdminRequest("GET", nil, "")
	if !ownedByOther(state, r, visOwnedNode()) {
		t.Error("an unreadable flag switched the ownership fence off")
	}
	if canManageNode(state, r, visOwnedNode()) {
		t.Error("an unreadable flag let an admin manage a customer's machine")
	}
}
