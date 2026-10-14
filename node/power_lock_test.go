package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/api/types/container"
	"github.com/redis/go-redis/v9"
)

// A stop and a start for one server ran side by side: the start's recreate
// removed the container while the stop was still saving. They now queue on the
// server's lock; another server is not held up, and kill never waits.
func TestPowerActionsForOneServerRunInTurn(t *testing.T) {
	release, _ := lockServerPower("a")
	if !serverPowerActive("a") || serverPowerActive("b") {
		t.Fatal("active flag wrong while a holds the lock")
	}
	otherDone := make(chan struct{})
	go func() { r, _ := lockServerPower("b"); r(); close(otherDone) }()
	select {
	case <-otherDone:
	case <-time.After(time.Second):
		t.Fatal("another server waited on a's lock")
	}

	second := make(chan struct{})
	go func() { r, _ := lockServerPower("a"); r(); close(second) }()
	select {
	case <-second:
		t.Fatal("a second action on a ran while the first held its lock")
	case <-time.After(100 * time.Millisecond):
	}
	// One waits; a third is dropped rather than taking another command slot.
	if _, ok := lockServerPower("a"); ok {
		t.Fatal("a third action queued behind a running and a waiting one")
	}
	release()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("the second action never ran after the first finished")
	}
	if serverPowerActive("a") {
		t.Fatal("a still reads active after both released")
	}
	if serialisedActions["kill"] || !serialisedActions["stop"] || !serialisedActions["switch_server"] {
		t.Fatal("kill must not wait behind a hanging stop; stop and switch must take the lock")
	}
}

// "stop" in the console exits the JVM cleanly while the node watches; a host
// reboot exits it cleanly too, but before this node started, and those servers
// must come back. A crash or an OOM kill is not a clean stop.
func TestACleanExitIsAStopOnlyIfTheNodeSawIt(t *testing.T) {
	started := time.Now()
	after := started.Add(time.Minute).Format(time.RFC3339Nano)
	before := started.Add(-time.Minute).Format(time.RFC3339Nano)
	for _, c := range []struct {
		name string
		st   *container.State
		want bool
	}{
		{"stop in the console", &container.State{ExitCode: 0, FinishedAt: after}, true},
		{"stopped by a reboot before the node started", &container.State{ExitCode: 0, FinishedAt: before}, false},
		{"a crash", &container.State{ExitCode: 1, FinishedAt: after}, false},
		{"OOM-killed", &container.State{ExitCode: 0, OOMKilled: true, FinishedAt: after}, false},
		{"still running", &container.State{Running: true, FinishedAt: after}, false},
		{"never started", &container.State{FinishedAt: "0001-01-01T00:00:00Z"}, false},
	} {
		if got := exitedCleanlyAfter(c.st, started); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// The marker holds the server down even against a Core that does not land it,
// and every deliberate start clears it along with reconcile_failed.
func TestAServerThatStoppedItselfStaysDownUntilStarted(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	st := &StorageManager{}
	if restartBlocked(ctx, rdb, st, "u1") {
		t.Fatal("blocked with no marker")
	}
	rdb.Set(ctx, stoppedByServerKey("u1"), "1", 0)
	rdb.Set(ctx, "dylaris:server:u1:reconcile_failed", "x", 0)
	if !restartBlocked(ctx, rdb, st, "u1") {
		t.Fatal("a server that stopped itself was not held down")
	}
	clearRunBlockers(ctx, rdb, "u1")
	if mr.Exists(stoppedByServerKey("u1")) || mr.Exists("dylaris:server:u1:reconcile_failed") {
		t.Fatal("a deliberate start left a marker holding the server down")
	}
}

// The wiring, named per site so moving one of them fails here by name.
func TestPowerWiringIsInPlace(t *testing.T) {
	read := func(f string) string {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	mainSrc, rec, dmSrc := read("main.go"), read("reconciler.go"), read("docker_mgr.go")
	proc := between(t, mainSrc, "func processCommand(", "case \"switch_server\":")
	if !strings.Contains(proc, "lockServerPower(cmd.Config.UUID)") {
		t.Error("processCommand does not take the server's power lock")
	}
	sw := between(t, mainSrc, "Switching server %s to sub-server", "case \"start\":")
	if !strings.Contains(sw, "clearRunBlockers(") {
		t.Error("switch_server starts the server without clearing what holds it down")
	}
	crash := after(t, rec, "func StartReconciler(")
	if !strings.Contains(crash, "serverPowerActive(") || !strings.Contains(crash, "stoppedItself(") {
		t.Error("the crash pass restarts during a power action or treats a clean stop as a crash")
	}
	clean := read("clean_stop.go")
	if !strings.Contains(clean, "crashReportSince(filepath.Join(") || !strings.Contains(clean, "return !logTailAsksRestart(") {
		t.Error("stoppedItself no longer checks for a crash report and a restart before keeping a server down")
	}
	for _, fn := range []string{"func (dm *DockerManager) RecreateWithCommand(", "func (dm *DockerManager) RecreateKeepingRunState("} {
		body := between(t, dmSrc, fn, "func (dm *DockerManager) PowerAction(")
		if fn == "func (dm *DockerManager) RecreateWithCommand(" {
			body = between(t, dmSrc, fn, "func (dm *DockerManager) RecreateKeepingRunState(")
		}
		if !strings.Contains(body, "dm.stopGracefully(") {
			t.Errorf("%s stops a running server without saving it first", fn)
		}
	}
}

// A clean exit after a crash or a server-side restart must come back: a crash
// report written since the start, or a restart in the last console lines.
func TestACleanExitAfterACrashOrARestartIsNotAStop(t *testing.T) {
	dir := t.TempDir()
	started := time.Now().Add(-time.Hour)
	if crashReportSince(dir, started) {
		t.Fatal("an empty crash-reports folder read as a crash")
	}
	if err := os.WriteFile(dir+"/crash-2026-10-13_12.00.00-server.txt", []byte("Ticking entity"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !crashReportSince(dir, started) {
		t.Fatal("a crash report written since the start was not seen")
	}
	if crashReportSince(dir, time.Now().Add(time.Hour)) {
		t.Fatal("a crash report older than the start read as this run's")
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	key := serverLogStreamKey("u1", "survival")
	// Ids are milliseconds on Redis's clock; the test sets them.
	ms := int64(1_000_000_000)
	add := func(line string) {
		ms++
		rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, ID: fmt.Sprintf("%d-0", ms), Values: map[string]interface{}{"line": line}})
	}
	add("[12:00:00] [Server thread/INFO]: Restarting in 5 minutes (an earlier session)")
	ms += (3 * time.Minute).Milliseconds() // the window ends before this
	add("[Server crashed (exit code 1) - restarting automatically (1/3)...]")
	add("[12:00:00] [Server thread/INFO]: <Steve> restart pls")
	add("[12:00:00] [Server thread/INFO]: [Not Secure] <Alex> restart?")
	add("[Server thread/INFO]: Stopping server")
	if logTailAsksRestart(ctx, rdb, key) {
		t.Fatal("a plain stop, player chat, the shipper's own crash line, or a line from before the window read as a restart")
	}
	add("[Server thread/INFO]: Startup script './start.sh' does not exist! Stopping server.")
	if !logTailAsksRestart(ctx, rdb, key) {
		t.Fatal("Paper's /restart without a script did not read as a restart")
	}
	// A spam after the restart line does not push it out of reach.
	for i := range 300 {
		add(fmt.Sprintf("[DH-World Gen Thread[0]/WARN]: spam %d", i))
	}
	if !logTailAsksRestart(ctx, rdb, key) {
		t.Fatal("a log spam hid the restart line")
	}
}
