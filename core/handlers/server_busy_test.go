package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
)

// Setup, reinstall, switching, sub-server copy and delete, the server delete
// and a move all ran beside a move or an install on the same server. Power,
// resources and restore already refused; the rest wrote into a directory that
// was being archived, rewritten or deleted from the other end.
func TestABusyServerRefusesWorkThatRewritesIt(t *testing.T) {
	setup := func(t *testing.T, srv *models.Server, busy string) *httptest.ResponseRecorder {
		fs := &serverPowerFakeStore{server: srv, node: &models.Node{ID: 7, Token: "node-token-1", Name: "n1", Status: "online"}}
		rdb := newServerPowerRedis(t)
		if busy != "" {
			rdb.Set(context.Background(), nodeBusyKey(srv.UUID), busy, 0)
		}
		h := newServerPowerHandler(fs, rdb)
		body, _ := json.Marshal(map[string]any{
			"subServerName": "main",
			"installer":     map[string]string{"type": "paper", "version": "1.21.1", "mcVersion": "1.21.1"},
		})
		r := httptest.NewRequest("POST", "/api/servers/1/setup", bytes.NewReader(body))
		r = mux.SetURLVars(r, map[string]string{"id": "1"})
		ctx := context.WithValue(r.Context(), "username", "alice")
		ctx = context.WithValue(ctx, "isAdmin", false)
		ctx = context.WithValue(ctx, "userID", "u1")
		rec := httptest.NewRecorder()
		h.SetupServer(rec, r.WithContext(ctx))
		return rec
	}
	srv := func(status string) *models.Server {
		return &models.Server{ID: 1, UUID: "srv-uuid", Status: status, OwnerID: "u1", OwnerName: "alice", NodeID: 7}
	}

	if rec := setup(t, srv("migrating"), ""); rec.Code != http.StatusConflict {
		t.Fatalf("setup while moving: %d %s", rec.Code, rec.Body.String())
	}
	if rec := setup(t, srv("stopped"), "restarting"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "still working") {
		t.Fatalf("setup while the node restores: %d %s", rec.Code, rec.Body.String())
	}
	if rec := setup(t, srv("stopped"), ""); rec.Code == http.StatusConflict {
		t.Fatalf("setup of an idle server was refused: %s", rec.Body.String())
	}
}

// Every handler that rewrites or removes a server's directory takes the guard,
// before it sends the node anything. Named per function, so moving the check
// out of one of them fails here by name.
func TestEveryRewritingHandlerTakesTheBusyGuard(t *testing.T) {
	for _, c := range []struct{ file, header, guard string }{
		{"servers_lifecycle.go", "SetupServer", "refuseIfServerBusy("},
		{"servers_lifecycle.go", "ReinstallServer", "refuseIfServerBusy("},
		{"servers_lifecycle.go", "SwitchSubServer", "refuseIfServerBusy("},
		{"servers_lifecycle.go", "DeleteSubServer", "refuseIfServerBusy("},
		{"servers_lifecycle.go", "queueMigration", "refuseIfServerBusy("},
		{"servers_lifecycle.go", "DeleteServer", "refuseIfMoving("},
		{"server_version_update.go", "CopySubServer", "refuseIfServerBusy("},
	} {
		body := funcBody(t, c.file, c.header)
		guard := strings.Index(body, c.guard)
		send := strings.Index(body, "SendCommand(")
		if send < 0 {
			send = strings.Index(body, "EnqueueMigration(")
		}
		if guard < 0 || (send >= 0 && send < guard) {
			t.Errorf("%s: %s missing or after the node is told", c.header, c.guard)
		}
	}
}

// A start clears what holds the server down itself. The node clears them only
// when it runs the command; until then the status watcher read them as unlanded
// and set the intent back to stopped under the start.
func TestAStartClearsWhatHoldsTheServerDown(t *testing.T) {
	fs := &serverPowerFakeStore{
		server: &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerID: "u1", OwnerName: "alice", NodeID: 7},
		node:   &models.Node{ID: 7, Token: "node-token-1", Name: "n1", Status: "online"},
	}
	rdb := newServerPowerRedis(t)
	ctx := context.Background()
	rdb.Set(ctx, "dylaris:server:srv-uuid:stopped_by_server", "1", 0)
	rdb.Set(ctx, "dylaris:server:srv-uuid:reconcile_failed", "x", 0)
	h := newServerPowerHandler(fs, rdb)
	rec := httptest.NewRecorder()
	h.ServerPowerHandler(rec, serverPowerReq(1, "start", "alice", false, "u1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	if n, _ := rdb.Exists(ctx, "dylaris:server:srv-uuid:stopped_by_server", "dylaris:server:srv-uuid:reconcile_failed").Result(); n != 0 {
		t.Fatalf("%d markers survived the start", n)
	}
}
