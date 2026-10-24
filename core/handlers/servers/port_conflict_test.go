package servers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// portConflictFakeStore embeds store.Store (nil) so it satisfies the interface
// at compile time; only what UpdateServerResources touches on this path is
// overridden. Anything else would panic, and these tests never reach one.
type portConflictFakeStore struct {
	store.Store

	usedPorts    []int
	usedPortsErr error
	portsUpdated bool
}

// The handler gates on LoadEffectivePermissions, which reads the user, not the
// "isAdmin" context value - so the fake has to answer both.
func (f *portConflictFakeStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, Role: "admin"}, nil
}

func (f *portConflictFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-1", NodeID: 7, HostPort: 25600, ContainerPort: 25565}, nil
}
func (f *portConflictFakeStore) UpdateServerResources(int, int, float64, int64) error { return nil }
func (f *portConflictFakeStore) GetUsedHostPortsOnNode(int) ([]int, error) {
	return f.usedPorts, f.usedPortsErr
}
func (f *portConflictFakeStore) UpdateServerPorts(int, int, int) error {
	f.portsUpdated = true
	return nil
}

// The happy path continues into audit logging; a disabled audit ends it there.
func (f *portConflictFakeStore) GetServerAuditState(int) (bool, bool, int, error) {
	return false, false, 0, nil
}

func (f *portConflictFakeStore) UpdateServerSetup(int, string, string, string, string, string, string, string) error {
	return nil
}

func portReq(t *testing.T, hostPort int) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"ram": 2048, "cpuLimit": 2.0, "diskLimit": 10240, "hostPort": hostPort,
	})
	r := httptest.NewRequest(http.MethodPut, "/api/servers/1/resources", bytes.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"id": "1"})
	ctx := context.WithValue(r.Context(), "isAdmin", true)
	ctx = context.WithValue(ctx, "userID", "admin-1")
	return r.WithContext(ctx)
}

// The host-port conflict check is the only thing standing between an admin and
// two servers on one node bound to the same port. It used to discard the
// lookup's error - `usedPorts, _ :=` - so a database hiccup produced an empty
// list, the loop found no conflict, and the port was assigned anyway. The
// server then failed to bind at start time, far from the action that caused it.
//
// A check that cannot run must REFUSE, not wave it through.
func TestUpdateServerResources_RefusesWhenThePortCheckFails(t *testing.T) {
	fake := &portConflictFakeStore{usedPortsErr: errors.New("connection reset")}
	h := &ServerHandler{state: &handlers.AppState{Store: fake}}

	rec := httptest.NewRecorder()
	h.UpdateServerResources(rec, portReq(t, 25601))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 - an unverifiable conflict check let the port through", rec.Code)
	}
	if fake.portsUpdated {
		t.Error("the port was assigned even though the conflict check never ran")
	}
}

// The check still has to do its job when it CAN run.
func TestUpdateServerResources_RejectsAPortAlreadyInUse(t *testing.T) {
	fake := &portConflictFakeStore{usedPorts: []int{25601, 25602}}
	h := &ServerHandler{state: &handlers.AppState{Store: fake}}

	rec := httptest.NewRecorder()
	h.UpdateServerResources(rec, portReq(t, 25601))

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	if fake.portsUpdated {
		t.Error("a conflicting port was assigned")
	}
}

// And must not block a free one.
func TestUpdateServerResources_AllowsAFreePort(t *testing.T) {
	fake := &portConflictFakeStore{usedPorts: []int{25602}}
	h := &ServerHandler{state: &handlers.AppState{Store: fake}}

	rec := httptest.NewRecorder()
	h.UpdateServerResources(rec, portReq(t, 25601))

	if !fake.portsUpdated {
		t.Errorf("a free port was not assigned (status %d): %s", rec.Code, rec.Body.String())
	}
}
