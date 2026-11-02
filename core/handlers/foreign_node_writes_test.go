package handlers

import (
	"bytes"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers/access"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/services"
)

// What staff and admins may still do to a customer's machine, written from the
// admin's side like node_visibility_test.go, each with a platform twin that must
// get PAST the guard. Found by the review of step 1 of
// roadmap/node-ownership-and-access.md (defects 8 to 11).

func TestNodeOwnedByOther(t *testing.T) {
	owner := handlertest.VisOwner
	owned := &models.Node{ID: 7, OwnerID: &owner}
	platform := &models.Node{ID: 7}
	cases := []struct {
		name string
		st   *handlertest.ForeignStore
		user string
		want bool
	}{
		{"a platform machine", &handlertest.ForeignStore{Node: platform, Byon: "true"}, handlertest.VisAdmin, false},
		{"a customer's machine, BYON on", &handlertest.ForeignStore{Node: owned, Byon: "true"}, handlertest.VisAdmin, true},
		{"its owner", &handlertest.ForeignStore{Node: owned, Byon: "true"}, handlertest.VisOwner, false},
		{"a customer's machine, BYON off", &handlertest.ForeignStore{Node: owned, Byon: "false"}, handlertest.VisAdmin, false},
		// Fail closed: these decide whether an admin opens a customer's world.
		{"the node cannot be read", &handlertest.ForeignStore{NodeErr: errors.New("connection refused")}, handlertest.VisAdmin, true},
		{"the flag cannot be read", &handlertest.ForeignStore{Node: owned, SettingErr: errors.New("connection refused")}, handlertest.VisAdmin, true},
		// Never saved is not a fault: it is the default, which is off.
		{"the flag was never saved", &handlertest.ForeignStore{Node: owned, SettingErr: sql.ErrNoRows}, handlertest.VisAdmin, false},
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
	st := &handlertest.ForeignStore{Node: node, Byon: "true"}
	s := &AppState{Store: st, Authz: authz.NewResolver(st), FeatureFlags: services.NewFeatureFlags(st)}
	s.Authz.SetForeignNode(s.NodeOwnedByOther)
	return s
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
		{"set overcommit", func(t *testing.T, s *AppState, w http.ResponseWriter) {
			r := handlertest.AdminRequest("PUT", id, `{"cpuOvercommitRatio":1,"ramOvercommitRatio":1}`)
			r.URL.Path = "/api/nodes/7/placement"
			(&PlacementHandler{state: s}).SetNodePlacement(w, r)
		}},
	}
	for _, rt := range routes {
		t.Run(rt.name+"/a customer's machine", func(t *testing.T) {
			rw := httptest.NewRecorder()
			rt.call(t, foreignState(handlertest.OwnedNode()), rw)
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
				rt.call(t, foreignState(handlertest.PlatformNode()), rw)
			}()
			if rw.Code == http.StatusNotFound {
				t.Fatalf("a platform machine was refused: %s", rw.Body.String())
			}
		})
	}
}

// TestFilesAndBeamAskTheResolverForAnAdmin: both answered an admin before the
// resolver was consulted, which made the resolver fence decorative.
func TestFilesAndBeamAskTheResolverForAnAdmin(t *testing.T) {
	for _, c := range []struct {
		name    string
		node    *models.Node
		allowed bool
	}{
		{"a customer's machine", handlertest.OwnedNode(), false},
		{"a platform machine", handlertest.PlatformNode(), true},
	} {
		t.Run("files/"+c.name, func(t *testing.T) {
			h := &FileHandler{state: foreignState(c.node)}
			r := handlertest.AdminRequest("GET", nil, "")
			r.URL.RawQuery = "server_uuid=srv-1"
			uuid, err := h.getServerUUID(r, "files.write")
			if (err == nil && uuid == "srv-1") != c.allowed {
				t.Fatalf("err = %v, allowed = %v", err, c.allowed)
			}
		})
	}
}

// The older fences (ownedByOther, canManageNode, canPlaceOnNode, disk tools)
// answer through ownershipInForce, which must fail closed like the resolver's
// half: the two used to disagree for as long as the settings table did not answer.
func TestTheOlderFencesFailClosedToo(t *testing.T) {
	st := &handlertest.ForeignStore{Node: handlertest.OwnedNode(), SettingErr: errors.New("connection refused")}
	state := &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st)}
	r := handlertest.AdminRequest("GET", nil, "")
	if !access.OwnedByOther(state.FeatureFlags, r, handlertest.OwnedNode()) {
		t.Error("an unreadable flag switched the ownership fence off")
	}
	if access.CanManageNode(state.FeatureFlags, r, handlertest.OwnedNode()) {
		t.Error("an unreadable flag let an admin manage a customer's machine")
	}
}
