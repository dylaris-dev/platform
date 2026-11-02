package access

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/store"
)

// The access checks took *AppState before they moved here and now take the
// dependencies they read. A state that was nil, or had a nil field, answered
// in a particular direction (fail closed for ownership, refuse for account
// management); these pin that each nil dependency still answers the same way.
func TestTheAccessChecksFailTheSameWayWithoutTheirDeps(t *testing.T) {
	owner := "tenant-1"
	owned := &models.Node{ID: 7, OwnerID: &owner}
	req := func(user string, admin bool) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		ctx := context.WithValue(r.Context(), "userID", user)
		return r.WithContext(context.WithValue(ctx, "isAdmin", admin))
	}

	if !NodeOwnedByOther(nil, nil, 7, owner) {
		t.Error("no store: a node that cannot be read must count as foreign")
	}
	if !ForeignToCaller(nil, nil, req(owner, true), 7) {
		t.Error("no store: ForeignToCaller must fail closed")
	}
	if !UserOwnedByOther(nil, &owner, "someone-else") {
		t.Error("no flags: somebody else's key must count as foreign")
	}
	if UserOwnedByOther(nil, &owner, owner) || KitOwnedByOther(nil, owner, owner) {
		t.Error("the owner is never a stranger to their own")
	}
	if OwnershipInForce(nil, req(owner, false)) || ByonActive(nil, req(owner, false)) || RouteOnlyActive(nil, req(owner, false)) {
		t.Error("no flags: no tenancy feature can be on")
	}
	// No flags means owner_id carries no meaning: operator territory.
	if CanPlaceOnNode(nil, req(owner, false), owned) || !CanPlaceOnNode(nil, req("admin", true), owned) {
		t.Error("with ownership off an owned node is placed on by an admin only")
	}
	if MayManageAccount(nil, nil, req("staff", false), &models.User{ID: "u2"}) {
		t.Error("no store or resolver: a non-admin must be refused, not waved through")
	}
	if !MayManageAccount(nil, nil, req("admin", true), &models.User{ID: "u2"}) {
		t.Error("an admin manages anyone, before any lookup")
	}
	if got := LoadEffectivePermissions(nil, nil, "u1"); got != (EffectivePermissions{Role: "user"}) {
		t.Errorf("no store: %+v", got)
	}
	if got := ApplyResolvedTabPermissions(nil, []models.Server{{ID: 1, Role: "invited"}}, "u1", "u"); len(got) != 1 {
		t.Error("no resolver: the list is returned as it came")
	}
}

// The routing-mode check is passed in now; it must still be asked only after
// the cheap early returns, so an admin's list or an empty one costs no read.
func TestRedactNodeAddressAsksTheRoutingModeLast(t *testing.T) {
	asked := 0
	gw := func() bool { asked++; return true }
	st := &handlertest.SettingsStore{KV: map[string]string{}}
	servers := []models.Server{{NodeID: 1, NodeAddress: "203.0.113.5"}}

	RedactNodeAddress(nil, gw, servers, false, "u1")
	RedactNodeAddress(st, gw, nil, false, "u1")
	RedactNodeAddress(st, gw, servers, true, "u1")
	if asked != 0 || servers[0].NodeAddress == "" {
		t.Fatalf("asked %d times, address %q", asked, servers[0].NodeAddress)
	}
	ownStore := &nodeStore{SettingsStore: st}
	RedactNodeAddress(ownStore, gw, servers, false, "u1")
	if asked != 1 || servers[0].NodeAddress != "" {
		t.Fatalf("asked %d times, address %q", asked, servers[0].NodeAddress)
	}
}

type nodeStore struct {
	*handlertest.SettingsStore
}

func (nodeStore) GetNodeByID(int) (*models.Node, error) { return &models.Node{ID: 1}, nil }

var _ store.Store = nodeStore{}
