package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/redis/go-redis/v9"
)

// stoppedItself reports whether a server that is down while the platform wants
// it up shut itself down on purpose - "stop" in the console or in game - so the
// reconciler leaves it stopped instead of restarting it as a crash.
//
// A clean exit alone does not say that. Minecraft exits 0 after many crashes
// too (it writes a crash report, then shuts down normally - "Ticking entity" on
// modded servers), and a server-side restart (Paper /restart without a restart
// script, auto-restart mods and plugins) exits 0 as well. Those must still come
// back, so a crash report written since the start, or a restart in the last log
// lines, reads as "restart it".
func (dm *DockerManager) stoppedItself(ctx context.Context, rdb *redis.Client, uuid string) bool {
	info, err := dm.cli.ContainerInspect(dm.ctx, "mc_"+uuid)
	if err != nil || !exitedCleanlyAfter(info.State, nodeStartedAt) || stoppedByDiskGuard(uuid, info.State) {
		return false
	}
	started, err := time.Parse(time.RFC3339Nano, info.State.StartedAt)
	if err != nil {
		return false
	}
	sub := ""
	if info.Config != nil {
		if s := strings.TrimPrefix(info.Config.WorkingDir, "/data/"); s != info.Config.WorkingDir {
			sub = s
		}
	}
	if crashReportSince(filepath.Join(dm.resolveLocalServerPath(uuid), sub, "crash-reports"), started) {
		return false
	}
	return !logTailAsksRestart(ctx, rdb, serverLogStreamKey(uuid, sub))
}

// diskGuardTrips is when the disk guard last stopped each server (uuid ->
// time.Time). Its stop exits 0 like "stop" in the console, and without this the
// reconciler read it as the server stopping itself once the hold was lifted,
// so a server whose owner freed space stayed down until someone pressed Start.
// Node memory only: after a node restart the stop predates nodeStartedAt and
// exitedCleanlyAfter already lets the server come back.
var diskGuardTrips sync.Map

// stoppedByDiskGuard reports whether st is the run the disk guard stopped:
// it was running when the guard tripped and ended after.
func stoppedByDiskGuard(uuid string, st *container.State) bool {
	v, ok := diskGuardTrips.Load(uuid)
	if !ok || st == nil {
		return false
	}
	at := v.(time.Time)
	started, err := time.Parse(time.RFC3339Nano, st.StartedAt)
	if err != nil {
		return false
	}
	finished, err := time.Parse(time.RFC3339Nano, st.FinishedAt)
	return err == nil && started.Before(at) && !finished.Before(at)
}

// crashReportSince reports whether dir holds a file written after t.
func crashReportSince(dir string, t time.Time) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.ModTime().After(t) {
			return true
		}
	}
	return false
}

// serverLogStreamKey is the log-shipper's console stream for a server.
func serverLogStreamKey(uuid, sub string) string {
	if sub != "" {
		return fmt.Sprintf("dylaris:server:%s:logs:%s", uuid, sub)
	}
	return fmt.Sprintf("dylaris:server:%s:logs", uuid)
}

// restartWindow is how much console output before the last line is read for
// a restart. A shutdown takes seconds, a modded one under load a minute or so.
// By time and not by line count: a log spam during the shutdown pushed the
// restart line out of the last 50 lines, and a server that asked to come back
// stayed down. Anchored on the newest line, whose id is Redis's clock - the
// node's clock can be minutes off it on a customer's machine.
const restartWindow = 2 * time.Minute

// logTailAsksRestart reports whether the console lines printed at the end ask
// for a restart: "/restart", an auto-restart plugin's announcement, or Paper's
// "Startup script ... does not exist! Stopping server." that /restart prints
// when no script is set. Player chat does not count - "<Steve> restart pls"
// must not bring back a server its owner stopped. Unreadable reads as "no",
// which keeps the stop.
func logTailAsksRestart(ctx context.Context, rdb *redis.Client, key string) bool {
	newest, err := rdb.XRevRangeN(ctx, key, "+", "-", 1).Result()
	if err != nil || len(newest) == 0 {
		return false
	}
	ms, err := strconv.ParseInt(strings.SplitN(newest[0].ID, "-", 2)[0], 10, 64)
	if err != nil {
		return false
	}
	from := strconv.FormatInt(ms-restartWindow.Milliseconds(), 10)
	msgs, err := rdb.XRevRangeN(ctx, key, "+", from, 5000).Result()
	if err != nil {
		return false
	}
	for _, m := range msgs {
		line, _ := m.Values["line"].(string)
		if chatLine(line) || shipperLine(line) {
			continue
		}
		msg := consoleMessage(line)
		if pluginLifecycleLine(msg) || playerChatLine(msg) {
			continue
		}
		if _, cmd, ok := strings.Cut(msg, " issued server command: "); ok {
			if restartCommand(cmd) {
				return true
			}
			continue
		}
		l := strings.ToLower(msg)
		if strings.Contains(l, "restart") || strings.Contains(l, "startup script") {
			return true
		}
	}
	return false
}

// consoleMessage is what a console line says, without its timestamp and thread
// and without the "[Plugin] " tags Bukkit puts in front of a plugin's lines: a
// plugin named "AutoRestart" carries that name in every line it logs, and a
// console stop of its server read as a restart and the server came back.
func consoleMessage(line string) string {
	msg := line
	if i := strings.Index(line, "]: "); i >= 0 {
		msg = line[i+3:]
	}
	for strings.HasPrefix(msg, "[") {
		j := strings.Index(msg, "] ")
		if j < 0 {
			break
		}
		msg = msg[j+2:]
	}
	return msg
}

// pluginLifecycleLine reports Bukkit's "Loading/Enabling/Disabling <Plugin>
// vX" lines, which name the plugin again and print on every start and stop.
func pluginLifecycleLine(msg string) bool {
	return strings.HasPrefix(msg, "Loading ") || strings.HasPrefix(msg, "Enabling ") || strings.HasPrefix(msg, "Disabling ")
}

// shipperLine reports a line the log-shipper wrote itself, not the server. Its
// "[Server crashed (exit code N) - restarting automatically ...]" says restart,
// and a stop typed within minutes of such a crash brought the server back.
func shipperLine(line string) bool {
	return strings.HasPrefix(line, "[Server crashed") || strings.HasPrefix(line, "[dylaris]") || strings.HasPrefix(line, "[log-shipper]")
}

// chatName is a chat line as plugins format it once consoleMessage took the
// rank tags off: "Steve: hi", "Steve » hi", "Steve > hi", with the "~" or "."
// nickname and Bedrock prefixes.
var chatName = regexp.MustCompile(`^[~.]?([A-Za-z0-9_]{3,16})(: | » | >>? )`)

// playerChatLine reports what a player wrote that is not vanilla chat: plugin
// formatted chat and /me ("* Steve hi"). A plugin announcing "Restart: in 10s"
// looks like chat by its shape, so a "name" that says restart itself is not one.
func playerChatLine(msg string) bool {
	if strings.HasPrefix(msg, "* ") {
		return true
	}
	m := chatName.FindStringSubmatch(msg)
	return m != nil && !strings.Contains(strings.ToLower(m[1]), "restart")
}

// restartCommand reports whether a player's command is a restart ("/restart",
// "/essentials:restart"), not one that only mentions it, like a /msg.
func restartCommand(cmd string) bool {
	name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(cmd), "/"), " ")
	name = strings.ToLower(name)
	return name == "restart" || strings.HasSuffix(name, ":restart")
}

// chatLine reports a player's chat message: "[12:00:00] [Server thread/INFO]:
// <Steve> hi", with 1.19+'s "[Not Secure] " in front of unsigned messages.
func chatLine(line string) bool {
	i := strings.Index(line, "]: ")
	if i < 0 {
		return false
	}
	msg := strings.TrimPrefix(line[i+3:], "[Not Secure] ")
	return strings.HasPrefix(msg, "<")
}
