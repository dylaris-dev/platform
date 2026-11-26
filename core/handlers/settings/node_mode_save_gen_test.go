package settings

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/handlers/handlertest"
)

// R57-05: every save that mirrors a key the node mode publisher also writes
// must bump the save generation the publisher WATCHes, or a tick that read
// before the commit overwrites the just-saved value until the next tick.
func TestSavesThatMirrorPublisherKeysBumpTheSaveGeneration(t *testing.T) {
	cases := []struct {
		name string
		body string
		save func(h *SettingsHandler, w http.ResponseWriter, r *http.Request)
	}{
		{"beam", `{"enabled":true}`, (*SettingsHandler).SaveBeamSettings},
		{"placement", `{"cpuOvercommitDefault":1,"ramOvercommitDefault":1}`, (*SettingsHandler).SavePlacementSettings},
		{"routing mode", `{"mode":"gateway","fileMode":"beam"}`, (*SettingsHandler).SaveRoutingMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			h := &SettingsHandler{d: Deps{Store: handlertest.NewSettingsStore(), Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()})}}
			rw := httptest.NewRecorder()
			tc.save(h, rw, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(tc.body))))
			if rw.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rw.Code, rw.Body.String())
			}
			if got, _ := mr.Get("dylaris:node_mode:save_gen"); got != "1" {
				t.Errorf("save generation = %q, want 1", got)
			}
		})
	}
}
