package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/services"
)

// A move between states raced the node's own work on the server: a restart
// recreating the container, a stop still saving. Online and stopped are what a
// move handles (it stops a running server first).
func TestAMoveWaitsForAPowerTransition(t *testing.T) {
	cases := []struct {
		status  string
		refused bool
	}{
		{"starting", true},
		{"restarting", true},
		{"stopping", true},
		{"online", false},
		{"stopped", false},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			srv := &models.Server{ID: 1, UUID: "srv-uuid", Status: c.status, OwnerID: "u1", NodeID: 7}
			fs := &serverPowerFakeStore{server: srv, node: &models.Node{ID: 7, Token: "node-token-1", Status: "online"}}
			rdb := newServerPowerRedis(t)
			h := newServerPowerHandler(fs, rdb)
			h.state.Migration = services.NewMigrationOrchestrator(fs, rdb, nil, nil, "")
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/x", nil)
			h.queueMigration(rec, r, srv, &models.Node{ID: 8, Status: "online"}, "manual", "alice")
			refused := rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "Move it when it has finished")
			if refused != c.refused {
				t.Fatalf("%s: status %d %s, want refused=%v", c.status, rec.Code, rec.Body.String(), c.refused)
			}
			if !c.refused && rec.Code != http.StatusAccepted {
				t.Fatalf("%s: status %d %s, want 202", c.status, rec.Code, rec.Body.String())
			}
		})
	}
}

// A copy walks the world files; a server booting, restarting or still saving
// on the way down is writing them.
func TestACopyWaitsForAPowerTransition(t *testing.T) {
	for _, status := range []string{"starting", "restarting", "stopping", "online", "installing"} {
		fs := &serverPowerFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid", Status: status, OwnerID: "u1", NodeID: 7, ActiveSubServer: "main"}}
		h := &ServerModsHandler{state: &AppState{Store: fs}}
		r := httptest.NewRequest("POST", "/api/servers/1/copy-sub-server", bytes.NewBufferString(`{"targetName":"copy"}`))
		r = mux.SetURLVars(r.WithContext(context.WithValue(r.Context(), "isAdmin", false)), map[string]string{"id": "1"})
		rec := httptest.NewRecorder()
		h.CopySubServer(rec, r)
		if rec.Code != http.StatusConflict {
			t.Errorf("copy while %s: status %d %s, want 409", status, rec.Code, rec.Body.String())
		}
	}
}
