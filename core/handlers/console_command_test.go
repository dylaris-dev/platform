package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
)

func sendConsole(t *testing.T, h *ConsoleHandler, cmd string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"command": cmd})
	req := httptest.NewRequest("POST", "/api/servers/1/console/command", strings.NewReader(string(body)))
	req = mux.SetURLVars(req, map[string]string{"id": "1"})
	rec := httptest.NewRecorder()
	h.SendCommand(rec, req)
	return rec.Code
}

// The input queue lives in the Redis every tenant shares, and nothing bounded
// it: any length, any count, and a stopped server never drained it - the
// commands then ran on its next start.
func TestConsoleCommandsAreBounded(t *testing.T) {
	fs := &serverPowerFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped"}}
	rdb := newServerPowerRedis(t)
	h := &ConsoleHandler{state: &AppState{Store: fs, Redis: rdb}}
	queue := "dylaris:server:srv-uuid:input"

	if code := sendConsole(t, h, "stop"); code != http.StatusConflict {
		t.Fatalf("a command to a stopped server: status %d, want 409", code)
	}
	if n := rdb.LLen(context.Background(), queue).Val(); n != 0 {
		t.Fatalf("%d command(s) queued for a stopped server", n)
	}

	fs.server.Status = "online"
	if code := sendConsole(t, h, strings.Repeat("a", consoleCommandMaxLen+1)); code != http.StatusBadRequest {
		t.Errorf("an overlong command: status %d, want 400", code)
	}
	if code := sendConsole(t, h, "say hi\x1b[2J"); code != http.StatusBadRequest {
		t.Errorf("a command with a control character: status %d, want 400", code)
	}
	for i := 0; i < consoleInputBacklog+50; i++ {
		if code := sendConsole(t, h, "list"); code != http.StatusOK {
			t.Fatalf("command %d: status %d", i, code)
		}
	}
	if n := rdb.LLen(context.Background(), queue).Val(); n != consoleInputBacklog {
		t.Fatalf("queue length %d, want it held at %d", n, consoleInputBacklog)
	}
}
