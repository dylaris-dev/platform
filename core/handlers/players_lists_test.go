package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

func TestKnownPlayersKeepsNameAndUUIDOnly(t *testing.T) {
	raw := json.RawMessage(`[{"name":"Dave","uuid":"u-1","expiresOn":"2026-11-01 10:00:00 +0000"},{"name":"Eve","uuid":"u-2","expiresOn":"x"},{"name":"Zed","uuid":"u-3"}]`)
	got := string(knownPlayers(raw, 2))
	if strings.Contains(got, "expiresOn") {
		t.Errorf("expiresOn reached the response: %s", got)
	}
	if got != `[{"name":"Dave","uuid":"u-1"},{"name":"Eve","uuid":"u-2"}]` {
		t.Errorf("knownPlayers = %s", got)
	}
	if got := string(knownPlayers(emptyJSONArray, maxKnownPlayers)); got != `[]` {
		t.Errorf("empty: got %s, want []", got)
	}
}

// playersListsStore serves one server and the demo list; everything else is
// the nil embedded Store and would panic if GetLists reached for it.
type playersListsStore struct {
	store.Store
	srv   *models.Server
	demos string
}

func (f *playersListsStore) GetServerByID(int) (*models.Server, error) { return f.srv, nil }
func (f *playersListsStore) GetSetting(key string) (string, error) {
	if key == demoServerUUIDsSetting {
		return f.demos, nil
	}
	return "", nil
}

func getListsAs(t *testing.T, demos string) playerListsResponse {
	t.Helper()
	srv := &models.Server{ID: 7, UUID: "srv-uuid", NodeID: 1, ActiveSubServer: "main"}
	state := &AppState{Store: &playersListsStore{srv: srv, demos: demos}, StoreEnabled: true}
	h := NewPlayersHandler(state)
	r := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/api/servers/7/players/lists", nil), map[string]string{"id": "7"})
	rw := httptest.NewRecorder()
	h.GetLists(rw, r)
	var out playerListsResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rw.Body.String())
	}
	return out
}

// A demo server is readable by every signed-in account, and players.read is
// one of the caps the demo grants. usercache is the same player identities the
// file browser hides from those viewers, so it is not even read for them.
// (No node registry here: every file read fails, so "known" showing up in
// Unavailable is the proof the read was attempted.)
func TestDemoStrangerGetsNoUsercache(t *testing.T) {
	demo := getListsAs(t, `["srv-uuid"]`)
	if _, tried := demo.Unavailable["known"]; tried {
		t.Error("usercache.json was read for a demo stranger")
	}
	if string(demo.Known) != `[]` {
		t.Errorf("known = %s, want []", demo.Known)
	}
	if _, tried := demo.Unavailable["bans"]; !tried {
		t.Error("the other lists should still be read")
	}

	normal := getListsAs(t, `[]`)
	if _, tried := normal.Unavailable["known"]; !tried {
		t.Error("usercache.json was not read on a server that is no demo")
	}
}

type playersAuditStore struct {
	store.Store
	audits []*models.ServerAuditEvent
}

func (f *playersAuditStore) GetServerAuditState(int) (bool, bool, int, error) {
	return true, false, 0, nil
}
func (f *playersAuditStore) InsertServerAudit(ev *models.ServerAuditEvent) error {
	f.audits = append(f.audits, ev)
	return nil
}

func TestPlayerActionsAreAudited(t *testing.T) {
	cases := []struct {
		action, player string
		wantPlayer     string
	}{
		{"whitelist_off", "", ""},
		{"whitelist_on", "ignored", ""},
		{"ban", "Herobrine", "Herobrine"},
		{"Whitelist_Add", " Dave ", "Dave"},
	}
	for _, c := range cases {
		fs := &playersAuditStore{}
		r := httptest.NewRequest(http.MethodPost, "/api/servers/7/players/action", nil)
		auditPlayerAction(&AppState{Store: fs}, r, 7, c.action, c.player, false)
		if len(fs.audits) != 1 {
			t.Fatalf("%s: %d audit rows, want 1", c.action, len(fs.audits))
		}
		ev := fs.audits[0]
		if ev.EventType != ServerAuditEventPlayerAction || ev.ServerID != 7 {
			t.Errorf("%s: row = %s on %d", c.action, ev.EventType, ev.ServerID)
		}
		if got := ev.Metadata["action"]; got != strings.ToLower(c.action) {
			t.Errorf("%s: metadata action = %v", c.action, got)
		}
		got, has := ev.Metadata["player"]
		if c.wantPlayer == "" && has {
			t.Errorf("%s: metadata carries a player %v", c.action, got)
		}
		if c.wantPlayer != "" && got != c.wantPlayer {
			t.Errorf("%s: metadata player = %v, want %s", c.action, got, c.wantPlayer)
		}
	}
}
