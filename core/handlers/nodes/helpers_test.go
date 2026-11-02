package nodes

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
)

// Copies of handlers test fixtures the node tests use. Test files cannot be
// imported across packages; each keeps its original name.

// foreignState: a customer's (BYON) machine, with the flag read the handlers
// ask. The node handlers hold no resolver; they ask access directly.
func foreignState(node *models.Node) Deps {
	st := &handlertest.ForeignStore{Node: node, Byon: "true"}
	return testDeps(Deps{Store: st, FeatureFlags: services.NewFeatureFlags(st)})
}

// The node half of handlers' TestPlacementAndForceDeleteFailClosed: a failed
// flag read must not let an admin force-delete a customer's machine.
func TestForceDeleteFailsClosedOnACustomersMachine(t *testing.T) {
	fault := errors.New("connection refused")

	t.Run("force delete a customer's machine", func(t *testing.T) {
		st := &orphanAssignFakeStore{node: handlertest.OwnedNode()}
		flags := &handlertest.ForeignStore{SettingErr: fault}
		h := &NodeHandler{d: testDeps(Deps{Store: st, FeatureFlags: services.NewFeatureFlags(flags)})}
		rec := httptest.NewRecorder()
		h.ForceDeleteNode(rec, handlertest.VisAdminRequest("DELETE", "/x", map[string]string{"id": "7"}))
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
		call func(t *testing.T, s Deps, w http.ResponseWriter)
	}{
		{"reset pairing", func(t *testing.T, s Deps, w http.ResponseWriter) {
			(&NodeAdmissionHandler{d: s}).ResetPairing(w, handlertest.AdminRequest("POST", id, ""))
		}},
		{"roll the key", func(t *testing.T, s Deps, w http.ResponseWriter) {
			(&NodeAdmissionHandler{d: s}).RollSecret(w, handlertest.AdminRequest("POST", id, ""))
		}},
		{"set the CPU pool", func(t *testing.T, s Deps, w http.ResponseWriter) {
			(&NodeHandler{d: s}).UpdateNode(w, handlertest.AdminRequest("PUT", id, `{}`))
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

// testDeps is routes.go's nodesDeps for tests: a core storage Manager over
// the same objects, as the AppState methods ran over them before S12.
func testDeps(d Deps) Deps {
	d.CoreStorage = &corestorage.Manager{Store: d.Store, Redis: d.Redis, ClusterSecret: d.ClusterSecret, Registry: d.GRPCRegistry}
	return d
}
