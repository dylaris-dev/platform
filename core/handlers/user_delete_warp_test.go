package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/store"
)

// Deleting an account has to hand the overlay teardown down to the service that
// does it. The service was tested and the call site was not, which is the shape
// that shipped a BYON release with no Link in any deploy file: everything the
// helper does is irrelevant if nobody passes it.

type userDeleteWarpStore struct {
	store.Store
	keys []store.WarpAPIKey
}

func (f *userDeleteWarpStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, Username: "victim"}, nil
}
func (f *userDeleteWarpStore) CountServersByOwner(string) (int, error) { return 0, nil }
func (f *userDeleteWarpStore) ListAllWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) {
	return f.keys, nil
}
func (f *userDeleteWarpStore) ListWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) {
	return f.keys, nil
}
func (f *userDeleteWarpStore) RevokeWarpAPIKeyByNodeID(string) error          { return nil }
func (f *userDeleteWarpStore) ListNodesByOwner(string) ([]models.Node, error) { return nil, nil }
func (f *userDeleteWarpStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return nil, nil
}
func (f *userDeleteWarpStore) DeleteUser(string) error   { return nil }
func (f *userDeleteWarpStore) CountAdmins() (int, error) { return 5, nil }

type userDeleteWarpPeers struct{ keyIDs []int }

func (p *userDeleteWarpPeers) DisconnectKeyPeers(_ context.Context, keyID int) int {
	p.keyIDs = append(p.keyIDs, keyID)
	return 1
}

func TestDeleteUser_HandsTheOverlayTeardownDown(t *testing.T) {
	const victimID = "3f1c2b8e-0d44-4a91-9a6f-1d2e3c4b5a60"
	fs := &userDeleteWarpStore{keys: []store.WarpAPIKey{{ID: 11, NodeID: "node-abc", OwnerID: victimID}}}
	peers := &userDeleteWarpPeers{}
	h := NewUserHandler(&AppState{Store: fs, WarpPeers: peers})

	r := httptest.NewRequest(http.MethodDelete, "/api/users/"+victimID, nil)
	ctx := context.WithValue(r.Context(), "userID", "admin-id")
	ctx = context.WithValue(ctx, "username", "admin")
	ctx = context.WithValue(ctx, "isAdmin", true)
	r = mux.SetURLVars(r.WithContext(ctx), map[string]string{"id": victimID})
	rec := httptest.NewRecorder()

	h.DeleteUser(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(peers.keyIDs) != 1 || peers.keyIDs[0] != 11 {
		t.Fatalf("disconnected %v, want [11] - the handler never passed the overlay teardown down, so the peers outlive the account", peers.keyIDs)
	}
}
