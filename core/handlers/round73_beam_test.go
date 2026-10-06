package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/pkg/crypto"
	"dylaris-core/store"
)

func mintBeamTicket(t *testing.T, h *BeamHandler, userID string) int {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/beam/ticket?server_uuid=srv-uuid", nil)
	ctx := context.WithValue(r.Context(), "username", "owner")
	ctx = context.WithValue(ctx, "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", userID)
	rec := httptest.NewRecorder()
	h.GetBeamTicket(rec, r.WithContext(ctx))
	return rec.Code
}

func beamTicketStore(t *testing.T, settings map[string]string) *beamAccessFakeStore {
	t.Helper()
	enc, err := crypto.Encrypt(crypto.DeriveKey("test-cluster-secret", "node-redis-secret"), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	fs := newBeamAccessStore(nil, "owner-id")
	fs.secretEnc = enc
	fs.settings = settings
	return fs
}

// The Beam page's off switch was read by the panel and nobody else: tickets
// kept coming and the node, which never sees the setting, honoured them.
func TestBeamTicketHonoursTheOffSwitch(t *testing.T) {
	for _, c := range []struct {
		enabled string
		want    int
	}{{"", 200}, {"true", 200}, {"false", 403}} {
		h := newBeamAccessHandler(beamTicketStore(t, map[string]string{"beam.enabled": c.enabled}))
		if got := mintBeamTicket(t, h, "owner-id"); got != c.want {
			t.Errorf("beam.enabled=%q: status %d, want %d", c.enabled, got, c.want)
		}
	}
}

// The demo gate refuses by HTTP method, and the ticket is a GET that carries
// write and delete rights the node enforces without knowing about demo.
func TestBeamTicketRefusesTheDemoAccount(t *testing.T) {
	for _, c := range []struct {
		name, demo string
		want       int
	}{{"demo account", "owner-id", 403}, {"another demo account", "someone-else", 200}, {"no demo", "", 200}} {
		h := newBeamAccessHandler(beamTicketStore(t, map[string]string{demoAccountUUIDSetting: c.demo}))
		h.state.StoreEnabled = true
		if got := mintBeamTicket(t, h, "owner-id"); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
}

type settingsReaderStore struct {
	*beamAccessFakeStore
	grant []string
}

func (s *settingsReaderStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{Grant: s.grant}, nil
}

// GET /settings/beam stays open to every user for `enabled`, and used to hand
// all of them the relay topology and every limit with it.
func TestBeamSettingsShowOnlyEnabledWithoutSettingsRead(t *testing.T) {
	cases := []struct {
		name  string
		admin bool
		grant []string
		full  bool
	}{
		{"plain user", false, nil, false},
		{"settings.read holder", false, []string{"settings.read"}, true},
		{"admin", true, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &settingsReaderStore{beamAccessFakeStore: beamTicketStore(t, map[string]string{
				"beam.enabled":          "true",
				"beam.public_host":      "beam.example.test",
				"beam.max_upload_bytes": "1024",
			}), grant: c.grant}
			h := NewSettingsHandler(&AppState{Store: fs, Authz: authz.NewResolver(fs)})

			r := httptest.NewRequest("GET", "/api/settings/beam", nil)
			ctx := context.WithValue(r.Context(), "username", "u")
			ctx = context.WithValue(ctx, "isAdmin", c.admin)
			ctx = context.WithValue(ctx, "userID", "u-id")
			rec := httptest.NewRecorder()
			h.GetBeamSettings(rec, r.WithContext(ctx))

			var body struct {
				Settings map[string]any `json:"settings"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
			}
			if body.Settings["enabled"] != true {
				t.Errorf("enabled missing: %v", body.Settings)
			}
			_, hasHost := body.Settings["publicHost"]
			_, hasCap := body.Settings["maxUploadBytes"]
			if hasHost != c.full || hasCap != c.full {
				t.Errorf("publicHost=%v maxUploadBytes=%v, want both %v", hasHost, hasCap, c.full)
			}
		})
	}
}
