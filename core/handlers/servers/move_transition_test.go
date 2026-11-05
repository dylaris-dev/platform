package servers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers/handlertest"

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
			rdb := handlertest.RedisClient(t)
			h := newServerPowerHandler(fs, rdb)
			h.d.Migration = services.NewMigrationOrchestrator(fs, rdb, nil, nil, "")
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
		h := &ServerModsHandler{d: testDeps(Deps{Store: fs})}
		r := httptest.NewRequest("POST", "/api/servers/1/copy-sub-server", bytes.NewBufferString(`{"targetName":"copy"}`))
		r = mux.SetURLVars(r.WithContext(context.WithValue(r.Context(), "isAdmin", false)), map[string]string{"id": "1"})
		rec := httptest.NewRecorder()
		h.CopySubServer(rec, r)
		if rec.Code != http.StatusConflict {
			t.Errorf("copy while %s: status %d %s, want 409", status, rec.Code, rec.Body.String())
		}
	}
}

// Without the gateway a proxy and its backends would be cut apart by a move,
// and an External node is unreachable: both refused before anything queues.
func TestADirectModeMoveRefusesWhatItCannotCarry(t *testing.T) {
	proxyID := 3
	cases := map[string]struct {
		srv    models.Server
		target models.Node
	}{
		"a proxy":               {srv: models.Server{ServerType: "proxy"}, target: models.Node{ID: 8, Status: "online"}},
		"a server behind one":   {srv: models.Server{ProxyID: &proxyID}, target: models.Node{ID: 8, Status: "online"}},
		"onto an External node": {target: models.Node{ID: 8, Status: "online", Tags: "external"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := c.srv
			srv.ID, srv.UUID, srv.Status, srv.OwnerID, srv.NodeID = 1, "srv-uuid", "stopped", "u1", 7
			fs := &serverPowerFakeStore{server: &srv, node: &models.Node{ID: 7, Token: "node-token-1", Status: "online"}}
			rdb := handlertest.RedisClient(t)
			h := newServerPowerHandler(fs, rdb)
			h.d.Migration = services.NewMigrationOrchestrator(fs, rdb, nil, nil, "")
			rec := httptest.NewRecorder()
			h.queueMigration(rec, httptest.NewRequest("POST", "/x", nil), &srv, &c.target, "manual", "alice")
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "move_needs_gateway") {
				t.Fatalf("status %d %s, want 409 move_needs_gateway", rec.Code, rec.Body.String())
			}
		})
	}
}
