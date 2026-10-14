package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	if err != nil || !exitedCleanlyAfter(info.State, nodeStartedAt) {
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
		l := strings.ToLower(line)
		if strings.Contains(l, "restart") || strings.Contains(l, "startup script") {
			return true
		}
	}
	return false
}

// shipperLine reports a line the log-shipper wrote itself, not the server. Its
// "[Server crashed (exit code N) - restarting automatically ...]" says restart,
// and a stop typed within minutes of such a crash brought the server back.
func shipperLine(line string) bool {
	return strings.HasPrefix(line, "[Server crashed") || strings.HasPrefix(line, "[dylaris]") || strings.HasPrefix(line, "[log-shipper]")
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
