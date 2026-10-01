package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
)

// Every roster read was an RCON round trip, with nothing in between. A fresh
// answer now serves the next few seconds of callers without reaching the
// server - this handler has no RCON wired at all, so reaching it would panic.
func TestOnlinePlayersAreServedFromTheShortCache(t *testing.T) {
	fs := &serverPowerFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid"}}
	rdb := newServerPowerRedis(t)
	rdb.Set(context.Background(), "dylaris:server:srv-uuid:players-online", `{"success":true,"output":"There are 2 of a max of 20 players online: a, b"}`, 0)
	h := &PlayersHandler{state: &AppState{Store: fs, Redis: rdb}}
	req := mux.SetURLVars(httptest.NewRequest("GET", "/api/servers/1/players/online", nil), map[string]string{"id": "1"})
	rec := httptest.NewRecorder()
	h.GetOnline(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "2 of a max of 20") {
		t.Fatalf("status %d body %s, want the cached roster", rec.Code, rec.Body.String())
	}
}
