package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
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
}

// A stop or kill that ran on its own workers before an earlier start reached a
// worker keeps the server stopped: the start that was queued first is dropped.
func TestStartOvertakenByAStop(t *testing.T) {
	uuid := "srv-overtaken"
	noteStop(uuid, 1000)
	noteStop(uuid, 900) // an older stop does not move the mark back
	tests := []struct {
		action string
		issued int64
		want   bool
	}{
		{"start", 999, true},
		{"restart", 500, true},
		{"switch_server", 999, true},
		{"start", 1000, false}, // the same second: order unknown
		{"start", 1001, false}, // queued after the stop
		{"start", 0, false},    // from a Core that does not stamp
		{"update_resources", 1, false},
	}
	for _, tt := range tests {
		if got := overtakenByStop(tt.action, uuid, tt.issued); got != tt.want {
			t.Errorf("%s issued %d: %v, want %v", tt.action, tt.issued, got, tt.want)
		}
	}
	if overtakenByStop("start", "another-server", 1) {
		t.Error("one server's stop dropped another server's start")
	}
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "} else if overtakenByStop(cmd.Action, cmd.Config.UUID, cmd.IssuedAt) {") {
		t.Error("processCommand no longer drops a start a stop has overtaken")
	}
}
