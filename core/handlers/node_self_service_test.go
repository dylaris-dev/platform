package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// selfNodeStore embeds store.Store (nil) so it satisfies the interface at
// compile time; only what these handlers touch is overridden. Anything else
// would panic, and these tests never reach one.
type selfNodeStore struct {
	store.Store
	node             *models.Node
	servers          []models.Server
	installs         map[int][]models.SubServerInstall
	deletedNode      int
	deletedServersOf int
}

func (f *selfNodeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.node == nil || f.node.ID != id {
		return nil, store.ErrNodeHasServers // any non-nil error: the handler only checks for one
	}
	return f.node, nil
}
func (f *selfNodeStore) ListServersByNode(int) ([]models.Server, error) { return f.servers, nil }
func (f *selfNodeStore) ListSubServerInstalls(id int) ([]models.SubServerInstall, error) {
	return f.installs[id], nil
}
func (f *selfNodeStore) DeleteNode(id int) error          { f.deletedNode = id; return nil }
func (f *selfNodeStore) DeleteServersByNode(id int) error { f.deletedServersOf = id; return nil }

func selfNodeReq(method, target, userID string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r = mux.SetURLVars(r, map[string]string{"id": "7"})
	//nolint:staticcheck // the handlers read a plain string key; matching them is the point
	return r.WithContext(context.WithValue(r.Context(), "userID", userID))
}

func ownedBy(uid string) *models.Node {
	return &models.Node{ID: 7, Name: "home-box", Token: "tok", Status: "online", OwnerID: &uid}
}

// The rule /me exists to enforce: this answers for MY machine and nothing else.
//
// It matters more here than on a read: the capability-gated DELETE /nodes/{id}
// has no ownership check of its own, so simply opening that route to tenants
// would have let any of them delete any node in the fleet. This is a separate,
// narrower surface precisely so that never has to be relaxed.
func TestMyNodeIsOwnerScoped(t *testing.T) {
	t.Run("the owner reaches it", func(t *testing.T) {
		h := &NodeHandler{state: &AppState{Store: &selfNodeStore{node: ownedBy("me")}}}
		w := httptest.NewRecorder()
		h.GetMyNodeContents(w, selfNodeReq("GET", "/api/me/nodes/7/contents", "me"))
		if w.Code != 200 {
			t.Fatalf("the owner got %d, want 200", w.Code)
		}
	})

	t.Run("another tenant does not", func(t *testing.T) {
		fs := &selfNodeStore{node: ownedBy("someone-else")}
		h := &NodeHandler{state: &AppState{Store: fs}}
		w := httptest.NewRecorder()
		h.DeleteMyNode(w, selfNodeReq("DELETE", "/api/me/nodes/7", "me"))
		// 404, not 403: on a /me route the difference would confirm that
		// somebody else's node exists to anyone who guessed an id.
		if w.Code != 404 {
			t.Errorf("a stranger got %d, want 404", w.Code)
		}
		if fs.deletedNode != 0 {
			t.Error("a stranger deleted somebody else's machine")
		}
	})

	// An ADMIN too. canManageNode would say yes here, which on a route called
	// /me would quietly mean "any node in the fleet"; admins keep the
	// capability-gated route for other people's machines.
	t.Run("an admin does not get in through this door", func(t *testing.T) {
		fs := &selfNodeStore{node: ownedBy("someone-else")}
		h := &NodeHandler{state: &AppState{Store: fs}}
		r := selfNodeReq("DELETE", "/api/me/nodes/7", "admin-id")
		r = r.WithContext(context.WithValue(r.Context(), "isAdmin", true)) //nolint:staticcheck
		w := httptest.NewRecorder()
		h.DeleteMyNode(w, r)
		if w.Code != 404 {
			t.Errorf("an admin got %d on somebody else's node, want 404", w.Code)
		}
		if fs.deletedNode != 0 {
			t.Error("an admin deleted another tenant's machine through the /me route")
		}
	})

	// A platform node has no owner at all. Nobody may reach it here.
	t.Run("an unowned node belongs to nobody", func(t *testing.T) {
		fs := &selfNodeStore{node: &models.Node{ID: 7, Name: "swarm-1"}}
		h := &NodeHandler{state: &AppState{Store: fs}}
		w := httptest.NewRecorder()
		h.DeleteMyNode(w, selfNodeReq("DELETE", "/api/me/nodes/7", "me"))
		if w.Code != 404 {
			t.Errorf("got %d on an unowned node, want 404", w.Code)
		}
	})

	// No identity on the request must not match an owner-less comparison.
	t.Run("an empty caller matches nothing", func(t *testing.T) {
		fs := &selfNodeStore{node: ownedBy("")}
		h := &NodeHandler{state: &AppState{Store: fs}}
		w := httptest.NewRecorder()
		h.DeleteMyNode(w, selfNodeReq("DELETE", "/api/me/nodes/7", ""))
		if w.Code != 404 {
			t.Errorf("got %d for an empty caller, want 404", w.Code)
		}
	})
}

// The servers go only when asked. Defaulting the other way would make "I am
// moving this machine" and "I am done with it" the same button.
func TestDeleteMyNodeOnlyTakesServersWhenAsked(t *testing.T) {
	t.Run("without the flag the servers stay", func(t *testing.T) {
		fs := &selfNodeStore{node: ownedBy("me")}
		h := &NodeHandler{state: &AppState{Store: fs}}
		h.DeleteMyNode(httptest.NewRecorder(), selfNodeReq("DELETE", "/api/me/nodes/7", "me"))
		if fs.deletedServersOf != 0 {
			t.Error("the servers were deleted without being asked for")
		}
	})

	t.Run("with the flag they go together", func(t *testing.T) {
		fs := &selfNodeStore{node: ownedBy("me")}
		h := &NodeHandler{state: &AppState{Store: fs}}
		h.DeleteMyNode(httptest.NewRecorder(), selfNodeReq("DELETE", "/api/me/nodes/7?servers=delete", "me"))
		if fs.deletedServersOf != 7 {
			t.Error("servers=delete did not take the servers")
		}
		if fs.deletedNode != 7 {
			t.Error("the machine itself was not removed")
		}
	})
}

// The confirmation names every world. A count is a number people click past,
// and the sub-server somebody forgot they had is the one they wanted.
func TestMyNodeContentsNamesEverySubServer(t *testing.T) {
	fs := &selfNodeStore{
		node:    ownedBy("me"),
		servers: []models.Server{{ID: 3, Name: "Survival", UUID: "u-3", ActiveSubServer: "main"}},
		installs: map[int][]models.SubServerInstall{
			3: {{SubServerName: "main"}, {SubServerName: "creative-test"}},
		},
	}
	h := &NodeHandler{state: &AppState{Store: fs}}
	w := httptest.NewRecorder()
	h.GetMyNodeContents(w, selfNodeReq("GET", "/api/me/nodes/7/contents", "me"))

	body := w.Body.String()
	for _, want := range []string{"Survival", "main", "creative-test"} {
		if !strings.Contains(body, want) {
			t.Errorf("the confirmation does not name %q: %s", want, body)
		}
	}
}

func (s *selfNodeStore) ListWarpKeyIDsBoundToNode(int) ([]int, error) { return nil, nil }
