package beam

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// visOwner, visAdmin, visOwnedNode, visPlatformNode: handlers/node_visibility_test.go.
const (
	visOwner = "11111111-1111-1111-1111-111111111111"
	visAdmin = "22222222-2222-2222-2222-222222222222"
)

func visOwnedNode() *models.Node {
	owner := visOwner
	return &models.Node{ID: 7, Status: "online", OwnerID: &owner}
}

func visPlatformNode() *models.Node { return &models.Node{ID: 7, Status: "online"} }

// The beam half of handlers' TestFilesAndBeamAskTheResolverForAnAdmin: an admin
// may beam to a platform machine, not to a customer's.
func TestBeamAsksTheResolverForAnAdmin(t *testing.T) {
	for _, c := range []struct {
		name    string
		node    *models.Node
		allowed bool
	}{
		{"a customer's machine", visOwnedNode(), false},
		{"a platform machine", visPlatformNode(), true},
	} {
		t.Run("beam/"+c.name, func(t *testing.T) {
			h := &BeamHandler{state: foreignState(c.node)}
			ok, perms := h.canBeam(foreignAdminRequest("GET", nil, ""), 3)
			if ok != c.allowed || perms.Write != c.allowed {
				t.Fatalf("canBeam = %v %+v, want %v", ok, perms, c.allowed)
			}
		})
	}
}

// Copies of handlers/foreign_node_writes_test.go's fixtures.
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

func (f *foreignFakeStore) GetPanelRole(int) (*store.PanelRole, error) { return nil, nil }

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

func foreignState(node *models.Node) *handlers.AppState {
	st := &foreignFakeStore{node: node, byon: "true"}
	s := &handlers.AppState{Store: st, Authz: authz.NewResolver(st), FeatureFlags: services.NewFeatureFlags(st)}
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
