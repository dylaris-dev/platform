package servers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type versionGuardStore struct {
	store.Store
	srv  models.Server
	mods []models.ServerMod
}

// The real-IP switch as these tests set it by hand: mode "on", which refuses.
func (f *versionGuardStore) GetServerProxyMode(int) (string, error) { return store.ProxyModeOn, nil }

func (f *versionGuardStore) GetServerByID(int) (*models.Server, error) { s := f.srv; return &s, nil }
func (f *versionGuardStore) ListServerMods(int, string) ([]models.ServerMod, error) {
	return f.mods, nil
}

// A version update reinstalls the server, so it is refused where the result
// would lock players out (Real player IP on, a target the node cannot
// configure) or bring a switched-off mod back on.
func TestVersionUpdateRefusesWhatItWouldBreak(t *testing.T) {
	cases := []struct {
		name     string
		srv      models.Server
		mods     []models.ServerMod
		body     string
		wantCode string
	}{
		{"real IP on, moving to Fabric",
			models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "paper", ProxyProtocol: true},
			nil, `{"minecraft":"1.21.4","loader":"fabric"}`, "proxy_protocol_on"},
		{"real IP on, moving to Paper 1.18",
			models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "paper", ProxyProtocol: true},
			nil, `{"minecraft":"1.18.2","loader":"paper"}`, "proxy_protocol_on"},
		{"a switched-off mod",
			models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "fabric"},
			[]models.ServerMod{{ID: 2, Title: "Lithium", FileName: "lithium.jar", Disabled: true}},
			`{"minecraft":"1.21.4","loader":"fabric"}`, "mods_switched_off"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewServerModsHandler(testDeps(Deps{Store: &versionGuardStore{srv: c.srv, mods: c.mods}}))
			req := httptest.NewRequest(http.MethodPost, "/api/servers/1/version-update", strings.NewReader(c.body))
			req = mux.SetURLVars(req, map[string]string{"id": "1"})
			rec := httptest.NewRecorder()
			h.VersionUpdate(rec, req)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), c.wantCode) {
				t.Fatalf("answered %d %s, want 409 %s", rec.Code, rec.Body.String(), c.wantCode)
			}
		})
	}
}

// A move the node cannot carry out is refused before Core writes anything:
// a loader with no installer behind it, or a server still being installed.
func TestVersionUpdateRefusesWhatTheNodeCannotDo(t *testing.T) {
	cases := []struct {
		name       string
		srv        models.Server
		body       string
		wantStatus int
		wantCode   string
	}{
		{"purpur", models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "purpur"},
			`{"minecraft":"1.21.4"}`, http.StatusUnprocessableEntity, "loader_not_installable"},
		{"quilt asked for", models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "fabric"},
			`{"minecraft":"1.21.4","loader":"quilt"}`, http.StatusUnprocessableEntity, "loader_not_installable"},
		{"spigot", models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "spigot"},
			`{"minecraft":"1.21.4"}`, http.StatusUnprocessableEntity, "loader_not_installable"},
		{"still installing", models.Server{ID: 1, ActiveSubServer: "main", Status: "installing", InstallerType: "paper"},
			`{"minecraft":"1.21.4"}`, http.StatusConflict, "server_installing"},
		{"being moved", models.Server{ID: 1, ActiveSubServer: "main", Status: "migrating", InstallerType: "paper"},
			`{"minecraft":"1.21.4"}`, http.StatusConflict, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewServerModsHandler(testDeps(Deps{Store: &versionGuardStore{srv: c.srv}}))
			req := httptest.NewRequest(http.MethodPost, "/api/servers/1/version-update", strings.NewReader(c.body))
			req = mux.SetURLVars(req, map[string]string{"id": "1"})
			rec := httptest.NewRecorder()
			h.VersionUpdate(rec, req)
			if rec.Code != c.wantStatus || !strings.Contains(rec.Body.String(), c.wantCode) {
				t.Fatalf("answered %d %s, want %d %s", rec.Code, rec.Body.String(), c.wantStatus, c.wantCode)
			}
		})
	}
}
