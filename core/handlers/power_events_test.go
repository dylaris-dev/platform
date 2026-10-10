package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A power action writes "starting"/"stopping" itself, but only a NODE-reported
// change made the status watcher publish servers.changed. The panel kept
// showing the old state until the node finished, so a stop looked ignored.
func TestPowerActionPublishesServersChangedOnce(t *testing.T) {
	tests := []struct {
		name   string
		status string
		action string
		code   int
		want   int
	}{
		{"start", "stopped", "start", http.StatusOK, 1},
		{"stop", "online", "stop", http.StatusOK, 1},
		{"restart", "online", "restart", http.StatusOK, 1},
		{"kill", "online", "kill", http.StatusOK, 1},
		{"refused stop on a stopped server", "stopped", "stop", http.StatusConflict, 0},
		{"refused start on a running server", "online", "start", http.StatusConflict, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := powerStateFixture(t, tc.status)
			rec := httptest.NewRecorder()
			got := countServersChanged(t, h.state, func() {
				h.ServerPowerHandler(rec, serverPowerReq(1, tc.action, "alice", false, "u1"))
			})
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.code, rec.Body.String())
			}
			if got != tc.want {
				t.Fatalf("published servers.changed %d time(s), want %d", got, tc.want)
			}
		})
	}
}
