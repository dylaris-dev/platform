package servers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// Install, uninstall and switching a mod on or off write the mods directory a
// version move, reinstall, restore or copy is working in. A jar written beside
// one of those is outside its stash, so a failed move rolled back to the old
// version and kept a jar built for the new one. They take the same guard their
// siblings do.
func TestModChangesRefuseWhileTheServerIsBusy(t *testing.T) {
	handlers := []struct {
		name   string
		method string
		body   string
		call   func(h *ServerModsHandler, w http.ResponseWriter, r *http.Request)
	}{
		{"install", http.MethodPost, `{"projectId":"spark","versionId":"v1"}`, (*ServerModsHandler).Install},
		{"uninstall", http.MethodDelete, ``, (*ServerModsHandler).Uninstall},
		{"set enabled", http.MethodPut, `{"enabled":false}`, (*ServerModsHandler).SetEnabled},
	}
	states := []struct {
		name   string
		status string
		busy   bool
	}{
		{"node busy", "stopped", true},
		{"still installing", "installing", false},
		{"being moved", "migrating", false},
	}
	for _, hc := range handlers {
		for _, sc := range states {
			t.Run(hc.name+"/"+sc.name, func(t *testing.T) {
				mr := miniredis.RunT(t)
				rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
				t.Cleanup(func() { rdb.Close() })
				if sc.busy {
					mr.Set(services.NodeBusyKey("u-1"), "installing")
				}
				fs := &versionGuardStore{
					srv:  models.Server{ID: 1, UUID: "u-1", NodeID: 3, ActiveSubServer: "main", Status: sc.status, InstallerType: "fabric", MinecraftVersion: "1.21.4"},
					mods: []models.ServerMod{{ID: 2, ServerID: 1, Title: "Spark", FileName: "spark.jar", TargetDir: "mods"}},
				}
				h := NewServerModsHandler(testDeps(Deps{Store: fs, Redis: rdb}))
				req := httptest.NewRequest(hc.method, "/api/servers/1/mods/2", strings.NewReader(hc.body))
				req = mux.SetURLVars(req, map[string]string{"id": "1", "modId": "2"})
				rec := httptest.NewRecorder()
				func() {
					defer func() {
						if p := recover(); p != nil {
							t.Fatalf("got past the guard and panicked: %v", p)
						}
					}()
					hc.call(h, rec, req)
				}()
				if rec.Code != http.StatusConflict {
					t.Fatalf("answered %d %s, want 409", rec.Code, rec.Body.String())
				}
			})
		}
	}
}
