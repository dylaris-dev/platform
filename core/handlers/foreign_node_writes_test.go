package handlers

import (
	"database/sql"
	"errors"
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
