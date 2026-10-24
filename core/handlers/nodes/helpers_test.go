package nodes

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime/debug"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Copies of handlers test fixtures the node tests use. Test files cannot be
// imported across packages; each keeps its original name.

// funcBody returns the text of one top-level func, from its signature to the
// closing brace in column 0.
func funcBody(t *testing.T, file, fn string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	re := regexp.MustCompile(`(?ms)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(fn) + `\(.*?^}`)
	body := re.FindString(string(src))
	if body == "" {
		t.Fatalf("%s not found in %s", fn, file)
	}
	return body
}

const (
	visOwner = "11111111-1111-1111-1111-111111111111"
	visAdmin = "22222222-2222-2222-2222-222222222222"
)

func visOwnedNode() *models.Node {
	owner := visOwner
	return &models.Node{ID: 7, Status: "online", OwnerID: &owner}
}

func visPlatformNode() *models.Node {
	return &models.Node{ID: 7, Status: "online"}
}

func visAdminRequest(method, target string, vars map[string]string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	ctx := context.WithValue(r.Context(), "isAdmin", true)
	ctx = context.WithValue(ctx, "userID", visAdmin)
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

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

// foreignState builds the state the way main does: the resolver asks the same
// predicate the handlers ask.
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

// The node half of handlers' TestPlacementAndForceDeleteFailClosed: a failed
// flag read must not let an admin force-delete a customer's machine.
func TestForceDeleteFailsClosedOnACustomersMachine(t *testing.T) {
	fault := errors.New("connection refused")

	t.Run("force delete a customer's machine", func(t *testing.T) {
		st := &orphanAssignFakeStore{node: visOwnedNode()}
		flags := &foreignFakeStore{settingErr: fault}
		h := &NodeHandler{state: &handlers.AppState{Store: st, FeatureFlags: services.NewFeatureFlags(flags)}}
		rec := httptest.NewRecorder()
		h.ForceDeleteNode(rec, visAdminRequest("DELETE", "/x", map[string]string{"id": "7"}))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
		}
	})
}

// The node rows of handlers' TestWritesOnACustomersMachineAreNotFound.
func TestNodeWritesOnACustomersMachineAreNotFound(t *testing.T) {
	id := map[string]string{"id": "7"}
	routes := []struct {
		name string
		call func(t *testing.T, s *handlers.AppState, w http.ResponseWriter)
	}{
		{"reset pairing", func(t *testing.T, s *handlers.AppState, w http.ResponseWriter) {
			(&NodeAdmissionHandler{state: s}).ResetPairing(w, foreignAdminRequest("POST", id, ""))
		}},
		{"roll the key", func(t *testing.T, s *handlers.AppState, w http.ResponseWriter) {
			(&NodeAdmissionHandler{state: s}).RollSecret(w, foreignAdminRequest("POST", id, ""))
		}},
		{"set the CPU pool", func(t *testing.T, s *handlers.AppState, w http.ResponseWriter) {
			(&NodeHandler{state: s}).UpdateNode(w, foreignAdminRequest("PUT", id, `{}`))
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

func tenancyFeatureFlags(byonEnabled bool) *services.FeatureFlags {
	v := "false"
	if byonEnabled {
		v = "true"
	}
	return services.NewFeatureFlags(tenancyFlagStore{val: v})
}

// tenancyFlagStore is a minimal settingsReader fake (services.FeatureFlags's
// unexported dependency interface - GetSetting(key string) (string, error))
// used to construct a real *services.FeatureFlags with the BYON flag pinned
// on or off, without needing a full store.Store fake.
type tenancyFlagStore struct {
	val string
}

func (f tenancyFlagStore) GetSetting(string) (string, error) { return f.val, nil }
