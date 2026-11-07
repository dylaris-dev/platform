package servers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// uninstallModFakeStore records whether the row was deleted, so a test can prove
// the handler refused BEFORE dropping the panel's only handle on the jar.
type uninstallModFakeStore struct {
	store.Store
	deleted bool
	mods    []models.ServerMod // nil: one installed plugin
}

// The suspension guard reads the owner's billing before anything is queued.
// An empty status is a paying account, which is what this case is about.
func (f *uninstallModFakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: userID}, nil
}

func (f *uninstallModFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-uuid", OwnerID: "alice", NodeID: 1, InstallerType: "paper"}, nil
}

func (f *uninstallModFakeStore) ListServerMods(serverID int, sub string) ([]models.ServerMod, error) {
	if f.mods != nil {
		return f.mods, nil
	}
	return []models.ServerMod{{ID: 5, ServerID: serverID, FileName: "some-plugin.jar", TargetDir: "plugins"}}, nil
}

func (f *uninstallModFakeStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id, Token: "node-token"}, nil
}

func (f *uninstallModFakeStore) DeleteServerMod(id, serverID int) error {
	f.deleted = true
	return nil
}

// The DB row is the only handle the panel has on an installed jar: the Content
// tab lists what the store returns, and the uninstall button is on that row. So
// dropping the row before the node has been told to delete the file leaves the
// jar loading on the server with no way left to remove it through the UI - the
// file manager is the only recovery.
//
// This path used to send the remove_mod command with `_ =` in front of it and
// delete the row whether or not the send worked, and to skip the node entirely
// when the queue was nil while still deleting the row. Install refuses on both
// of those; uninstall now does too.
func TestUninstallRefusesRatherThanDroppingTheRowWithNoQueue(t *testing.T) {
	fs := &uninstallModFakeStore{}
	h := &ServerModsHandler{d: testDeps(Deps{Store: fs})} // Queue deliberately nil
	rw := httptest.NewRecorder()

	req := httptest.NewRequest(http.MethodDelete, "/api/servers/7/mods/5", nil)
	req = req.WithContext(context.WithValue(req.Context(), "userID", "alice"))
	h.Uninstall(rw, mux.SetURLVars(req, map[string]string{"id": "7", "modId": "5"}))

	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when the node cannot be reached (body: %s)", rw.Code, rw.Body.String())
	}
	if fs.deleted {
		t.Error("the mod row was deleted even though the node was never told to remove the file; " +
			"the jar keeps loading and the panel no longer lists it")
	}
}

// Remove on a row whose jar is still downloading deleted the row and queued a
// remove_mod that found nothing yet; the download then renamed the jar into
// place with no row left to remove it with. A row whose report never came is
// still removable once the install cannot be running any more.
func TestUninstallWaitsForAnInstallInFlight(t *testing.T) {
	cases := []struct {
		name    string
		age     time.Duration
		refused bool
	}{
		{"just queued", time.Minute, true},
		{"report lost long ago", 2 * modInstallInFlightFor, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &uninstallModFakeStore{mods: []models.ServerMod{{ID: 5, ServerID: 7, FileName: "some-plugin.jar",
				TargetDir: "plugins", Status: models.ServerModInstalling, InstalledAt: time.Now().Add(-c.age)}}}
			h := &ServerModsHandler{d: testDeps(Deps{Store: fs})} // no queue: past the guard it answers 503
			rw := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodDelete, "/api/servers/7/mods/5", nil)
			req = req.WithContext(context.WithValue(req.Context(), "userID", "alice"))
			h.Uninstall(rw, mux.SetURLVars(req, map[string]string{"id": "7", "modId": "5"}))

			if refused := rw.Code == http.StatusConflict; refused != c.refused {
				t.Fatalf("status %d %s, want refused=%v", rw.Code, rw.Body.String(), c.refused)
			}
			if fs.deleted {
				t.Error("the row was deleted")
			}
		})
	}
}
