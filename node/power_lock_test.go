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
	release, _ := lockServerPower("a", "start", nil)
	if !serverPowerActive("a") || serverPowerActive("b") {
		t.Fatal("active flag wrong while a holds the lock")
	}
	otherDone := make(chan struct{})
	go func() { r, _ := lockServerPower("b", "start", nil); r(); close(otherDone) }()
	select {
	case <-otherDone:
	case <-time.After(time.Second):
		t.Fatal("another server waited on a's lock")
	}

	second := make(chan struct{})
	go func() { r, _ := lockServerPower("a", "update_resources", nil); r(); close(second) }()
	select {
	case <-second:
		t.Fatal("a second action on a ran while the first held its lock")
	case <-time.After(100 * time.Millisecond):
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

// The user's last click wins: a stop behind a running action and a waiting
// start was refused and acked, and the start then brought the server up.
func TestTheNewestPowerActionReplacesTheWaitingOne(t *testing.T) {
	for i, c := range []struct{ running, waiting, newest string }{
		{"restart", "start", "stop"},
		{"update_resources", "start", "stop"},
		{"restart", "stop", "start"},
		{"reconfigure", "restart", "stop"},
		// The newest save of a kind holds everything the waiting one would
		// have applied (update_resources by merging, see below).
		{"restart", "update_resources", "update_resources"},
		{"update_resources", "reconfigure", "reconfigure"},
		{"reconfigure", "switch_server", "switch_server"},
	} {
		uuid := fmt.Sprintf("newest-%d", i)
		release, ok := lockServerPower(uuid, c.running, nil)
		if !ok {
			t.Fatalf("%v: the first action did not get the lock", c)
		}
		waiter := make(chan bool, 1)
		go func() {
			r, ok := lockServerPower(uuid, c.waiting, nil)
			if ok {
				r()
			}
			waiter <- ok
		}()
		time.Sleep(50 * time.Millisecond)
		newest := make(chan bool, 1)
		go func() {
			r, ok := lockServerPower(uuid, c.newest, nil)
			if ok {
				r()
			}
			newest <- ok
		}()
		select {
		case ok := <-waiter:
			if ok {
				t.Fatalf("%v: the replaced %s still ran", c, c.waiting)
			}
		case <-time.After(time.Second):
			t.Fatalf("%v: the replaced %s still holds a command slot", c, c.waiting)
		}
		release()
		select {
		case ok := <-newest:
			if !ok {
				t.Fatalf("%v: the newest %s was dropped", c, c.newest)
			}
		case <-time.After(time.Second):
			t.Fatalf("%v: the newest %s never ran", c, c.newest)
		}
	}
}

// A waiting change is replaced only by a newer one of its own kind: a config
// change of another kind, or a power action, waits beside it and both run. The
// three config kinds shared one slot, and a third change was dropped and acked.
func TestAPowerActionDoesNotReplaceAWaitingConfigChange(t *testing.T) {
	release, _ := lockServerPower("cfg", "restart", nil)
	actions := []string{"update_resources", "reconfigure", "switch_server", "stop"}
	done := make(chan bool, len(actions))
	for _, a := range actions {
		a := a
		go func() {
			r, ok := lockServerPower("cfg", a, nil)
			if ok {
				r()
			}
			done <- ok
		}()
		time.Sleep(50 * time.Millisecond)
	}
	release()
	for range actions {
		select {
		case ok := <-done:
			if !ok {
				t.Fatal("an action was dropped although it had nothing to repeat")
			}
		case <-time.After(time.Second):
			t.Fatal("an action never ran")
		}
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
	if !strings.Contains(proc, "lockServerPower(cmd.Config.UUID, cmd.Action, &cmd.Config)") {
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

// An update_resources carries padding and ports only when they change, so a
// newer save that replaced a waiting one dropped the waiting one's change and
// acked it. The newer one now takes over what it does not set itself.
func TestANewerResourceSaveKeepsTheWaitingOnesChanges(t *testing.T) {
	pad := func(n int) *int { return &n }
	for i, c := range []struct {
		waiting, newest  DockerConfig
		wantPad          *int
		wantHost, wantCt int
	}{
		{DockerConfig{RAMPaddingMB: pad(1024), HostPort: 25601, ContainerPort: 25577}, DockerConfig{CPULimit: 2},
			pad(1024), 25601, 25577},
		{DockerConfig{RAMPaddingMB: pad(1024), HostPort: 25601}, DockerConfig{RAMPaddingMB: pad(256), HostPort: 25602, ContainerPort: 25566},
			pad(256), 25602, 25566},
		{DockerConfig{}, DockerConfig{CPULimit: 2}, nil, 0, 0},
	} {
		uuid := fmt.Sprintf("merge-%d", i)
		release, _ := lockServerPower(uuid, "restart", nil)
		waiting := &ServerConfig{UUID: uuid, Docker: c.waiting}
		dropped := make(chan bool, 1)
		go func() {
			r, ok := lockServerPower(uuid, "update_resources", waiting)
			if ok {
				r()
			}
			dropped <- !ok
		}()
		time.Sleep(50 * time.Millisecond)
		newest := &ServerConfig{UUID: uuid, Docker: c.newest}
		ran := make(chan struct{})
		go func() { r, _ := lockServerPower(uuid, "update_resources", newest); r(); close(ran) }()
		if !<-dropped {
			t.Fatalf("case %d: the waiting save was not replaced", i)
		}
		release()
		<-ran
		d := newest.Docker
		if (d.RAMPaddingMB == nil) != (c.wantPad == nil) || (d.RAMPaddingMB != nil && *d.RAMPaddingMB != *c.wantPad) ||
			d.HostPort != c.wantHost || d.ContainerPort != c.wantCt || d.CPULimit != c.newest.CPULimit {
			t.Errorf("case %d: newest ran with %+v (padding %v), want padding %v host %d container %d",
				i, d, d.RAMPaddingMB, c.wantPad, c.wantHost, c.wantCt)
		}
	}
}

// A kill pressed while a start or restart is already past the lock must keep
// the server down: the start used to recreate the container after it.
func TestAKillDuringAStartKeepsTheServerDown(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	for _, action := range []string{"start", "restart", "switch_server"} {
		uuid := "killed-during-" + action
		cmd := NodeCommand{Action: action, IssuedAt: 2000}
		cmd.Config.UUID = uuid
		if abandonedForStop(ctx, rdb, cmd) {
			t.Fatalf("%s abandoned with no stop or kill", action)
		}
		noteStop(uuid, 2001) // the kill, on its express worker
		if !abandonedForStop(ctx, rdb, cmd) {
			t.Fatalf("%s went on after a newer kill", action)
		}
		if got, _ := rdb.Get(ctx, "dylaris:server:"+uuid+":status").Result(); got != "stopped" {
			t.Errorf("%s abandoned with status %q, want stopped", action, got)
		}
	}
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, s := range []struct{ from, to, create string }{
		{"case \"switch_server\":", "case \"start\":", "dm.RecreateWithCommand(cmd.Config)"},
		{"case \"start\":", "case \"stop\":", "dm.RestartContainerInZone("},
		{"case \"restart\":", "case \"update_resources\":", "dm.RestartContainerInZone("},
	} {
		body := between(t, src, s.from, s.to)
		check, create := strings.Index(body, "abandonedForStop(ctx, rdb, cmd)"), strings.Index(body, s.create)
		if check < 0 || create < 0 || check > create {
			t.Errorf("%s brings the container up without checking for a kill first", s.from)
		}
	}
}

// Bukkit names a plugin in every line it logs and in its enable and disable
// lines, so a console stop of a server with "AutoRestart" read as a restart
// and the server came back.
func TestAPluginNamedRestartDoesNotTurnAStopIntoARestart(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	ms := int64(1_000_000_000)
	for i, c := range []struct {
		lines []string
		want  bool
	}{
		{[]string{
			"[12:00:00 INFO]: [SpigotLibraryLoader] [UltimateAutoRestart] Loading 1 libraries... please wait",
			"[12:00:00 INFO]: [UltimateAutoRestart] Loading server plugin UltimateAutoRestart v1.0",
			"[12:00:00 INFO]: [UltimateAutoRestart] Enabling UltimateAutoRestart v1.0",
			"[12:00:01] [Server thread/INFO]: [AutoRestart] Config loaded",
			"[12:00:02] [Server thread/INFO]: Stopping server",
			"[12:00:02] [Server thread/INFO]: [UltimateAutoRestart] Disabling UltimateAutoRestart v1.0",
		}, false},
		{[]string{
			"[12:00:00] [Server thread/INFO]: [AutoRestart] The server will restart in 10 seconds",
			"[12:00:10] [Server thread/INFO]: [AutoRestart] Disabling AutoRestart v2.1",
		}, true},
		{[]string{"[12:00:00] [Server thread/INFO]: Steve issued server command: /restart"}, true},
	} {
		key := serverLogStreamKey(fmt.Sprintf("plugin-%d", i), "")
		for _, l := range c.lines {
			ms++
			rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, ID: fmt.Sprintf("%d-0", ms), Values: map[string]interface{}{"line": l}})
		}
		if got := logTailAsksRestart(ctx, rdb, key); got != c.want {
			t.Errorf("case %d: asks restart = %v, want %v", i, got, c.want)
		}
	}
}
