package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
)

func powerStateFixture(t *testing.T, status string) (*ServerHandler, *serverPowerFakeStore) {
	t.Helper()
	fs := &serverPowerFakeStore{
		server: &models.Server{ID: 1, UUID: "srv-uuid", Status: status, OwnerID: "u1", OwnerName: "alice", NodeID: 7},
		node:   &models.Node{ID: 7, Token: "node-token-1", Name: "n1", Status: "online"},
	}
	return newServerPowerHandler(fs, newServerPowerRedis(t)), fs
}

func queuedPowerCommands(t *testing.T, h *ServerHandler) int64 {
	t.Helper()
	n, err := h.state.Redis.XLen(context.Background(), "dylaris:node:node-token-1:cmds").Result()
	if err != nil {
		return 0
	}
	return n
}

// The node runs power commands in parallel with an install, a restore or a
// move and nothing orders them: a start went into a half-installed directory,
// and a stop or kill was undone when the install ended by starting the server.
// While the node holds its busy key for the server, Core refuses - an operator
// included, because the node would undo the action for them too.
func TestPowerActionsWaitWhileTheNodeIsBusy(t *testing.T) {
	for _, admin := range []bool{false, true} {
		for _, action := range []string{"start", "stop", "restart", "kill"} {
			status := "online"
			if action == "start" {
				status = "stopped"
			}
			h, fs := powerStateFixture(t, status)
			h.state.Redis.Set(context.Background(), nodeBusyKey("srv-uuid"), "installing", 0)

			rec := httptest.NewRecorder()
			h.ServerPowerHandler(rec, serverPowerReq(1, action, "alice", admin, "u1"))
			if rec.Code != http.StatusConflict {
				t.Errorf("admin=%v %s while busy: status %d, want 409: %s", admin, action, rec.Code, rec.Body.String())
			}
			if n := queuedPowerCommands(t, h); n != 0 {
				t.Errorf("admin=%v %s while busy: %d command(s) queued", admin, action, n)
			}
			if len(fs.updateDesiredStateCalls) != 0 {
				t.Errorf("admin=%v %s while busy: desired state written %v", admin, action, fs.updateDesiredStateCalls)
			}
		}
	}
}

func TestPowerActionsWaitForAMoveUnlessOperator(t *testing.T) {
	h, _ := powerStateFixture(t, "migrating")
	rec := httptest.NewRecorder()
	h.ServerPowerHandler(rec, serverPowerReq(1, "start", "alice", false, "u1"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("start while migrating: status %d, want 409", rec.Code)
	}

	h, _ = powerStateFixture(t, "migrating")
	rec = httptest.NewRecorder()
	h.ServerPowerHandler(rec, serverPowerReq(1, "kill", "root", true, "admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("an operator could not act on a stuck move: status %d: %s", rec.Code, rec.Body.String())
	}
}

// The node's reconciler reads desired_state from Redis, which Core used to
// refresh only on its next scan tick. A kill landing in that window met a
// reconciler still reading "online" and a stopped container, and it started
// the server again.
func TestKillPublishesTheDesiredStateAtOnce(t *testing.T) {
	h, _ := powerStateFixture(t, "online")
	rec := httptest.NewRecorder()
	h.ServerPowerHandler(rec, serverPowerReq(1, "kill", "alice", false, "u1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("kill: status %d: %s", rec.Code, rec.Body.String())
	}
	got, err := h.state.Redis.Get(context.Background(), "dylaris:server:srv-uuid:desired_state").Result()
	if err != nil || got != "stopped" {
		t.Fatalf("desired_state in Redis = %q (%v), want stopped right after the kill", got, err)
	}
}

// A setup refused after its first checks used to have written the database
// already: the server sat "installing" with no install on its way.
func TestRefusedSetupWritesNothing(t *testing.T) {
	h, fs, _ := newInstallTest(t)
	rec := httptest.NewRecorder()
	// Modpacks are off in this fixture, so a pack is refused with 403.
	h.SetupServer(rec, installRequest("setup", setupBody(map[string]string{"type": "pack"}), false))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if fs.setupWrites != 0 {
		t.Fatalf("the refused setup wrote the server's setup %d time(s)", fs.setupWrites)
	}
	for _, c := range fs.updateStatusCalls {
		if c.status == "installing" {
			t.Fatal("the refused setup marked the server installing")
		}
	}
}

// With BYON off - the default - any signed-in user could create a server on
// any node, with any memory and no plan to limit it. canPlaceOnNode already
// said such nodes are operator territory; create never asked it then.
func TestCreateOnAPlatformNodeIsForOperatorsWhenBYONIsOff(t *testing.T) {
	h, _ := powerStateFixture(t, "stopped")
	body := `{"uuid":"0f0e0d0c-0b0a-4908-8706-050403020100","name":"x","nodeId":"7","docker":{"ram":500000,"cpuLimit":0,"diskLimit":0}}`
	r := httptest.NewRequest("POST", "/api/servers", strings.NewReader(body))
	ctx := context.WithValue(r.Context(), "username", "mallory")
	ctx = context.WithValue(ctx, "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", "u9")
	rec := httptest.NewRecorder()
	h.CreateServer(rec, r.WithContext(ctx))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a non-admin create on a platform node with BYON off: status %d, want 403: %s", rec.Code, rec.Body.String())
	}
}
