package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A power action queued while the node was away runs on its return only if it
// still matches what the server should be.
func TestStalePowerAction(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	old := now.Add(-3 * time.Hour).Unix()
	fresh := now.Add(-10 * time.Second).Unix()
	missing := errors.New("redis: nil")
	tests := []struct {
		name, action, desired string
		issued                int64
		derr                  error
		want                  bool
	}{
		{"old start, server now wanted off", "start", "stopped", old, nil, true},
		{"old start, still wanted on", "start", "online", old, nil, false},
		{"old restart, server now wanted off", "restart", "stopped", old, nil, true},
		{"old stop, still wanted off: carried out however late", "stop", "stopped", old, nil, false},
		{"old stop, server wanted on again", "stop", "online", old, nil, true},
		{"old kill, server wanted on again", "kill", "online", old, nil, true},
		{"old stop, intent not one we know: carried out", "stop", "suspended", old, nil, false},
		{"fresh start before Core published the intent", "start", "stopped", fresh, nil, false},
		{"no desired state to compare (Core away)", "start", "", old, missing, false},
		{"from a Core that does not stamp", "start", "stopped", 0, nil, false},
		{"not a power action", "backup_run", "online", old, nil, false},
	}
	for _, tt := range tests {
		if got := stalePowerAction(tt.action, tt.issued, now, tt.desired, tt.derr); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}

// Pinned in the source: processCommand needs Redis and Docker to run.
func TestStalePowerActionIsChecked(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "if stalePowerAction(cmd.Action, cmd.IssuedAt, heartbeatClock(rdb, id).now(ctx), desired, derr) {") {
		t.Error("processCommand no longer checks queued power actions against the current intent")
	}
}

func TestExpressCommands(t *testing.T) {
	for payload, want := range map[string]bool{
		`{"action":"kill","config":{"uuid":"s"}}`:  true,
		`{"action":"stop"}`:                        true,
		`{"action":"backup_run","serverUuid":"s"}`: false,
		`{"action":"start"}`:                       false,
		`not json`:                                 false,
	} {
		if got := isExpressCommand([]byte(payload)); got != want {
			t.Errorf("%s: %v, want %v", payload, got, want)
		}
	}
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "consumer.Express = isExpressCommand") {
		t.Error("the command consumer no longer gives stop and kill their own workers")
	}
	// Kill also has workers stops cannot take (see queue.Consumer.Instant).
	for payload, want := range map[string]bool{
		`{"action":"kill","config":{"uuid":"s"}}`: true,
		`{"action":"stop"}`:                       false,
		`not json`:                                false,
	} {
		if got := isKillCommand([]byte(payload)); got != want {
			t.Errorf("isKillCommand %s: %v, want %v", payload, got, want)
		}
	}
	if !strings.Contains(string(b), "consumer.Instant = isKillCommand") {
		t.Error("the command consumer no longer gives kill workers of its own")
	}
}

// A stop or kill that ran on its own workers before an earlier start reached a
// worker keeps the server stopped: the start that was queued first is dropped.
func TestStartOvertakenByAStop(t *testing.T) {
	uuid := "srv-overtaken"
	noteStop(uuid, 1000, 0)
	noteStop(uuid, 900, 0) // an older stop does not move the mark back
	tests := []struct {
		action string
		issued int64
		want   bool
	}{
		{"start", 999, true},
		{"restart", 500, true},
		{"switch_server", 999, false}, // still switched, left stopped (processCommand)
		{"start", 1000, false},        // the same second: order unknown
		{"start", 1001, false},        // queued after the stop
		{"start", 0, false},           // from a Core that does not stamp
		{"update_resources", 1, false},
	}
	for _, tt := range tests {
		if got := overtakenByStop(tt.action, uuid, tt.issued, 0); got != tt.want {
			t.Errorf("%s issued %d: %v, want %v", tt.action, tt.issued, got, tt.want)
		}
	}
	if overtakenByStop("start", "another-server", 1, 0) {
		t.Error("one server's stop dropped another server's start")
	}
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "} else if overtakenByStop(cmd.Action, cmd.Config.UUID, cmd.IssuedAt, cmd.IssuedAtMs) {") {
		t.Error("processCommand no longer drops a start a stop has overtaken")
	}
}

// A kill in the same second as a start or restart left the bring-up running:
// the order was read from whole seconds. With milliseconds on both sides the
// order is known; an older Core without them keeps the seconds rule.
func TestStopOrderUsesMillisecondsWhenBothCarryThem(t *testing.T) {
	tests := []struct {
		name            string
		stopS, stopMs   int64
		startS, startMs int64
		want            bool
	}{
		{"same second, kill later by ms", 2000, 2000400, 2000, 2000100, true},
		{"same second, kill earlier by ms", 2000, 2000100, 2000, 2000400, false},
		{"same millisecond", 2000, 2000100, 2000, 2000100, false},
		{"old core stop, same second", 2000, 0, 2000, 2000100, false},
		{"old core start, same second", 2000, 2000400, 2000, 0, false},
		{"old core both, later second", 2001, 0, 2000, 0, true},
		{"start without a stamp", 2000, 2000400, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uuid := "srv-ms-" + tt.name
			noteStop(uuid, tt.stopS, tt.stopMs)
			t.Cleanup(func() { lastStopIssued.Delete(uuid) })
			if got := overtakenByStop("start", uuid, tt.startS, tt.startMs); got != tt.want {
				t.Errorf("overtakenByStop = %v, want %v", got, tt.want)
			}
			cmd := NodeCommand{Action: "restart", IssuedAt: tt.startS, IssuedAtMs: tt.startMs}
			cmd.Config.UUID = uuid
			if got := abandonedForStop(context.Background(), redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()}), cmd); got != tt.want {
				t.Errorf("abandonedForStop = %v, want %v", got, tt.want)
			}
		})
	}
	uuid := "srv-ms-monotonic"
	t.Cleanup(func() { lastStopIssued.Delete(uuid) })
	noteStop(uuid, 2000, 2000400)
	noteStop(uuid, 2000, 2000100) // an older kill does not move the mark back
	if !overtakenByStop("start", uuid, 2000, 2000300) {
		t.Error("an older kill moved the millisecond mark back")
	}
}
