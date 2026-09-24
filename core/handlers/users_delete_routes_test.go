package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// A user's route-only addresses are the one thing they own that nothing removed
// with them. core_link_routes.owner_id is TEXT with no constraint - it neither
// cascades nor blocks - and RepublishCoreOwnedRoutes writes every stored row
// back into Redis every 60 seconds. So a deleted user's protected address kept
// routing players to their link indefinitely, and came back within the minute if
// anyone cleared the Redis key by hand.

type deleteRoutesFakeStore struct {
	store.Store
	routes     []store.CoreLinkRoute
	routesErr  error
	deleted    bool
	deleteUser error
}

func (f *deleteRoutesFakeStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, Username: "customer"}, nil
}
func (f *deleteRoutesFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return f.routes, f.routesErr
}
func (f *deleteRoutesFakeStore) DeleteUser(string) error { f.deleted = true; return f.deleteUser }

// Removing an account is on the record now, so the fake has to take the row.
func (f *deleteRoutesFakeStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }

type deleteRoutesFakeGateway struct {
	services.GatewayProvider
	removed []string
	err     error
}

// Asked by the account teardown before it destroys anything; this test is
// about a different question, so the answers are empty.
func (f *deleteRoutesFakeStore) CountServersByOwner(string) (int, error)        { return 0, nil }
func (f *deleteRoutesFakeStore) ListNodesByOwner(string) ([]models.Node, error) { return nil, nil }
func (f *deleteRoutesFakeStore) ListWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) {
	return nil, nil
}
func (f *deleteRoutesFakeStore) ListAllWarpAPIKeysByOwner(o string) ([]store.WarpAPIKey, error) {
	return f.ListWarpAPIKeysByOwner(o)
}

func (g *deleteRoutesFakeGateway) DeleteCoreOwnedRoute(domain string) error {
	if g.err != nil {
		return g.err
	}
	g.removed = append(g.removed, domain)
	return nil
}

const delUser = "11111111-1111-1111-1111-111111111111"

func TestDeleteUserRemovesTheirRouteOnlyAddresses(t *testing.T) {
	fs := &deleteRoutesFakeStore{routes: []store.CoreLinkRoute{
		{Domain: "mine-a.eu.dylaris.com", OwnerID: delUser},
		{Domain: "somebody-else.eu.dylaris.com", OwnerID: "22222222-2222-2222-2222-222222222222"},
		{Domain: "mine-b.eu.dylaris.com", OwnerID: delUser},
	}}
	gw := &deleteRoutesFakeGateway{}
	h := &UserHandler{state: &AppState{Store: fs, Gateway: gw}}

	rr := httptest.NewRecorder()
	h.DeleteUser(rr, deleteUserRequest())

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	if len(gw.removed) != 2 {
		t.Fatalf("removed %v, want exactly this user's two addresses", gw.removed)
	}
	for _, d := range gw.removed {
		if d == "somebody-else.eu.dylaris.com" {
			t.Fatalf("removed an address belonging to another account: %v", gw.removed)
		}
	}
	if !fs.deleted {
		t.Error("the user was never deleted")
	}
}

// The routes go FIRST and a failure there stops the delete. Removing the user
// first would strand the routes with no owner left to find them by.
func TestDeleteUserKeepsTheAccountWhenARouteCannotBeRemoved(t *testing.T) {
	fs := &deleteRoutesFakeStore{routes: []store.CoreLinkRoute{
		{Domain: "mine.eu.dylaris.com", OwnerID: delUser},
	}}
	gw := &deleteRoutesFakeGateway{err: errors.New("redis down")}
	h := &UserHandler{state: &AppState{Store: fs, Gateway: gw}}

	rr := httptest.NewRecorder()
	h.DeleteUser(rr, deleteUserRequest())

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if fs.deleted {
		t.Error("the account was deleted even though its addresses are still routing")
	}
}

// A user with no addresses must not be blocked by this, and an install with no
// gateway wired must still be able to delete users at all.
func TestDeleteUserWithNoRoutes(t *testing.T) {
	for _, tc := range []struct {
		name string
		gw   services.GatewayProvider
	}{
		{"gateway present, nothing owned", &deleteRoutesFakeGateway{}},
		{"no gateway configured", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &deleteRoutesFakeStore{}
			h := &UserHandler{state: &AppState{Store: fs, Gateway: tc.gw}}
			rr := httptest.NewRecorder()
			h.DeleteUser(rr, deleteUserRequest())
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
			}
			if !fs.deleted {
				t.Error("the user was never deleted")
			}
		})
	}
}
