package servers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPowerTransitionRefusal(t *testing.T) {
	cases := []struct {
		action, status string
		override       bool
		refused        bool
	}{
		{"start", "starting", false, true},
		{"stop", "starting", false, true},
		{"restart", "starting", false, true},
		{"stop", "stopping", false, true},
		{"start", "stopping", false, true},
		{"restart", "restarting", false, true},
		{"stop", "restarting", false, true},
		// Kill is the way out of a transition that hangs.
		{"kill", "starting", false, false},
		{"kill", "stopping", false, false},
		{"kill", "restarting", false, false},
		// An operator can clear a status a dead node left behind.
		{"start", "stopping", true, false},
		{"stop", "online", false, false},
		{"start", "stopped", false, false},
		{"restart", "online", false, false},
	}
	for _, c := range cases {
		msg := powerTransitionRefusal(c.action, c.status, c.override)
		if (msg != "") != c.refused {
			t.Errorf("%s while %s (override=%v): refusal %q, want refused=%v", c.action, c.status, c.override, msg, c.refused)
		}
	}
	if got, want := powerTransitionRefusal("stop", "starting", false), "The server is starting. Wait until it has finished, or use Force kill."; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// Through the handler: a stop clicked while the server boots is refused with a
// 409 and nothing reaches the node; kill still goes out.
func TestPowerActionsRefusedDuringATransition(t *testing.T) {
	for _, status := range []string{"starting", "stopping", "restarting"} {
		for _, action := range []string{"start", "stop", "restart"} {
			h, fs := powerStateFixture(t, status)
			rec := httptest.NewRecorder()
			h.ServerPowerHandler(rec, serverPowerReq(1, action, "alice", false, "u1"))
			if rec.Code != http.StatusConflict {
				t.Errorf("%s while %s: status %d, want 409: %s", action, status, rec.Code, rec.Body.String())
			}
			if n := queuedPowerCommands(t, h); n != 0 || len(fs.updateStatusCalls) != 0 {
				t.Errorf("%s while %s: %d command(s) queued, status writes %v", action, status, n, fs.updateStatusCalls)
			}
		}
		h, _ := powerStateFixture(t, status)
		rec := httptest.NewRecorder()
		h.ServerPowerHandler(rec, serverPowerReq(1, "kill", "alice", false, "u1"))
		if rec.Code != http.StatusOK {
			t.Errorf("kill while %s: status %d, want 200: %s", status, rec.Code, rec.Body.String())
		}
	}
}

// Two power actions a moment apart: the second is refused with a 429 while the
// lock lives, kill is never held by it, and the lock expires on its own.
func TestPowerLockRefusesTheSecondAction(t *testing.T) {
	h, _ := powerStateFixture(t, "online")
	rec := httptest.NewRecorder()
	h.ServerPowerHandler(rec, serverPowerReq(1, "restart", "alice", false, "u1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("first restart: status %d: %s", rec.Code, rec.Body.String())
	}
	ttl, err := h.state.Redis.TTL(context.Background(), powerLockKey("srv-uuid")).Result()
	if err != nil || ttl <= 0 || ttl > powerLockTTL {
		t.Fatalf("power lock TTL = %v (%v), want (0, %v]", ttl, err, powerLockTTL)
	}

	// The status the first action wrote went to the fake store, so the row
	// still reads online: only the lock stands between this and a second command.
	rec = httptest.NewRecorder()
	h.ServerPowerHandler(rec, serverPowerReq(1, "stop", "alice", false, "u1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second action under the lock: status %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeErrBody(t, rec); msg != "A power action was just sent; wait a moment" {
		t.Fatalf("message = %q", msg)
	}
	if n := queuedPowerCommands(t, h); n != 1 {
		t.Fatalf("%d commands queued, want 1", n)
	}

	rec = httptest.NewRecorder()
	h.ServerPowerHandler(rec, serverPowerReq(1, "kill", "alice", false, "u1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("kill under the lock: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestTakePowerLock(t *testing.T) {
	rdb := newServerPowerRedis(t)
	ctx := context.Background()
	if !takePowerLock(ctx, rdb, "a") {
		t.Fatal("first take refused")
	}
	if takePowerLock(ctx, rdb, "a") {
		t.Fatal("second take while held succeeded")
	}
	if !takePowerLock(ctx, rdb, "b") {
		t.Fatal("the lock of one server held another")
	}
	if !takePowerLock(ctx, nil, "a") {
		t.Fatal("no Redis must not refuse")
	}
	broken := newServerPowerRedis(t)
	broken.Close()
	if !takePowerLock(ctx, broken, "a") {
		t.Fatal("a Redis error must let the action through")
	}
	if powerLockTTL >= 5*time.Second {
		t.Fatalf("powerLockTTL = %v; the panel's own pause is 5 s and the lock must not outlast it", powerLockTTL)
	}
}
