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
	if err != nil || !exitedCleanlyAfter(info.State, nodeStartedAt) || stoppedByNode(uuid, info.State) {
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
	// The stream holds every run of the server, so only lines since this run
	// started may ask for a restart. StartedAt is this machine's clock and the
	// line ids are Redis's, hence the offset. ponytail: the offset is only
	// applied past redisClockTolerance, so up to 10s of skew remains.
	since := started.Add(heartbeatClock(rdb, nodeID).now(ctx).Sub(time.Now()))
	return !logTailAsksRestart(ctx, rdb, serverLogStreamKey(uuid, sub), since)
}

// nodeStops is when this node last stopped each server itself (uuid ->
// time.Time): gracefulStop for a setup, reinstall, version move, restart or
// the disk guard. Such a stop exits 0 like "stop" in the console, and without
// this the reconciler read it as the server stopping itself, so a server whose
// reinstall failed, or whose owner freed space after a disk guard trip, stayed
// down until someone pressed Start. A stop Core asked for flips desired_state
// to "stopped" first, so the reconciler never asks about it.
// Node memory only: after a node restart the stop predates nodeStartedAt and
// exitedCleanlyAfter already lets the server come back.
var nodeStops sync.Map

// stoppedByNode reports whether st is the run this node stopped: it was
// running when the stop began and ended after.
func stoppedByNode(uuid string, st *container.State) bool {
	v, ok := nodeStops.Load(uuid)
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
// which keeps the stop. Lines before since (Redis's clock; zero for no bound)
// are an earlier run's and do not count.
func logTailAsksRestart(ctx context.Context, rdb *redis.Client, key string, since time.Time) bool {
	newest, err := rdb.XRevRangeN(ctx, key, "+", "-", 1).Result()
	if err != nil || len(newest) == 0 {
		return false
	}
	ms, err := strconv.ParseInt(strings.SplitN(newest[0].ID, "-", 2)[0], 10, 64)
	if err != nil {
		return false
	}
	fromMs := ms - restartWindow.Milliseconds()
	if !since.IsZero() && since.UnixMilli() > fromMs {
		fromMs = since.UnixMilli()
	}
	msgs, err := rdb.XRevRangeN(ctx, key, "+", strconv.FormatInt(fromMs, 10), 5000).Result()
	if err != nil {
		return false
	}
	sayers := map[string]bool{}
	for _, m := range msgs {
		line, _ := m.Values["line"].(string)
		if name, cmd, ok := strings.Cut(consoleMessage(line), " issued server command: "); ok && sayCommand(cmd) {
			sayers[name] = true
		}
	}
	for _, m := range msgs {
		line, _ := m.Values["line"].(string)
		if chatLine(line) || shipperLine(line) || sayLine(line, sayers) {
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

// sayCommand reports a player's /say ("/say", "/minecraft:say").
func sayCommand(cmd string) bool {
	name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(cmd), "/"), " ")
	name = strings.ToLower(name)
	return name == "say" || strings.HasSuffix(name, ":say")
}

// sayLine reports a /say broadcast: "[Server thread/INFO]: [Steve] text",
// "[Server] text" from the console, "[Not Secure] [Steve] text" when unsigned.
// Its "[Steve] " looks like a plugin's tag, so a bare player-name tag counts
// only for a player seen issuing /say (sayers): a restart plugin's "[Name] "
// announcement must still bring the server back. ponytail: vanilla logs no
// issued commands, so its signed player /say is not caught.
func sayLine(line string, sayers map[string]bool) bool {
	i := strings.Index(line, "]: ")
	if i < 0 {
		return false
	}
	msg := line[i+3:]
	if strings.HasPrefix(msg, "[Not Secure] [") {
		return true
	}
	name, _, ok := strings.Cut(strings.TrimPrefix(msg, "["), "] ")
	return ok && strings.HasPrefix(msg, "[") && (name == "Server" || sayers[name])
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
