package settings

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/store"
	beamauth "dylaris-pkg/beam/auth"
)

type beamServersStore struct {
	*handlertest.SettingsStore
	servers []models.Server
}

func (s *beamServersStore) ListAllServers() ([]models.Server, error) { return s.servers, nil }

// Turning Beam off only stopped NEW tickets: the node never sees the setting,
// so a ticket already minted and every open session kept file access for up to
// a day. Flipping it off stamps every server, which the node already re-reads
// at Authenticate and on every session operation.
func TestSaveBeamSettings_TurningBeamOffStampsEveryServer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		before    string // stored beam.enabled before the save ("" = never saved, which is on)
		enabled   bool
		wantStamp bool
	}{
		{"on to off", "true", false, true},
		{"never saved (on) to off", "", false, true},
		// Every save with Beam off stamps, not just the flip: a stamp that failed
		// (Redis blip, a later key failing to save) is retried by saving again.
		{"off to off", "false", false, true},
		{"on to on", "true", true, false},
		{"off to on", "false", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			fs := &beamServersStore{SettingsStore: handlertest.NewSettingsStore(),
				servers: []models.Server{{UUID: "srv-a"}, {UUID: "srv-b"}, {UUID: ""}}}
			if tc.before != "" {
				fs.KV["beam.enabled"] = tc.before
			}
			h := &SettingsHandler{d: Deps{Store: fs, Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()})}}

			before := time.Now()
			body, _ := json.Marshal(BeamSettings{Enabled: tc.enabled})
			rw := httptest.NewRecorder()
			h.SaveBeamSettings(rw, httptest.NewRequest(http.MethodPost, "/api/settings/beam", bytes.NewReader(body)))
			if rw.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rw.Code, rw.Body.String())
			}
			for _, uuid := range []string{"srv-a", "srv-b"} {
				if got := mr.Exists(beamauth.AccessEpochKey(uuid)); got != tc.wantStamp {
					t.Errorf("%s stamped = %v, want %v", uuid, got, tc.wantStamp)
				}
				if !tc.wantStamp {
					continue
				}
				// Another replica answers beam.enabled from a snapshot up to
				// store.ReadCacheTTL old, and mints with iat = now. The stamp has
				// to cover every ticket such a stale "on" can still sign.
				v, _ := mr.Get(beamauth.AccessEpochKey(uuid))
				stamp, _ := strconv.ParseInt(v, 10, 64)
				if min := before.Add(store.ReadCacheTTL).Unix(); stamp < min {
					t.Errorf("%s stamp = %d, want >= %d (a ticket a stale replica mints still passes)", uuid, stamp, min)
				}
			}
		})
	}
}
