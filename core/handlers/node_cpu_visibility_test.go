package handlers

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// TestNodeCPUDoesNotLetResourceRightsReachACustomersMachine: every admin also
// carries CanChangeResources, so refusing the admin in canManageNode alone would
// have been undone by the second arm of the same condition.
func TestNodeCPUDoesNotLetResourceRightsReachACustomersMachine(t *testing.T) {
	admin := &models.User{ID: visAdmin, IsAdmin: true, Role: "admin"}
	cases := []struct {
		name string
		node *models.Node
		want int
	}{
		{"a user's machine", visOwnedNode(), http.StatusForbidden},
		// Past the gate, stopped by the nil pinning service.
		{"a platform machine", visPlatformNode(), http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &AppState{
				Store:        &orphanAssignFakeStore{node: c.node, user: admin},
				FeatureFlags: tenancyFeatureFlags(true),
			}
			h := &CPUPinningHandler{state: st}
			rw := httptest.NewRecorder()
			h.GetNodeCPU(rw, visAdminRequest("GET", "/x", map[string]string{"id": "7"}))
			if rw.Code != c.want {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, c.want, rw.Body.String())
			}
		})
	}
}

// orphanAssignFakeStore embeds store.Store (nil) so it satisfies the interface
// at compile time. Only the node lookup and GetServerByUUID are reached: every
// case below is decided by the node check or the duplicate check, before any
// user or server is created. node_visibility_test.go reuses it.
type orphanAssignFakeStore struct {
	store.Store
	server *models.Server
	err    error
	// node is what GetNodeByID answers; nil means a plain platform node, which
	// is what the duplicate-check cases below need to get past the node check.
	node *models.Node
	user *models.User
}

func (f *orphanAssignFakeStore) GetServerByUUID(string) (*models.Server, error) {
	return f.server, f.err
}

func (f *orphanAssignFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.node != nil {
		return f.node, nil
	}
	return &models.Node{ID: id, Status: "online"}, nil
}

func (f *orphanAssignFakeStore) ListServersByNode(int) ([]models.Server, error) {
	return nil, nil
}

func (f *orphanAssignFakeStore) GetUserByID(string) (*models.User, error) {
	if f.user == nil {
		return nil, sql.ErrNoRows
	}
	return f.user, nil
}
