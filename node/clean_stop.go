package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// logTailAsksRestart reports whether the server's last console lines ask for a
// restart: "/restart", an auto-restart plugin's announcement, or Paper's
// "Startup script ... does not exist! Stopping server." that /restart prints
// when no script is set. Unreadable reads as "no", which keeps the stop.
func logTailAsksRestart(ctx context.Context, rdb *redis.Client, key string) bool {
	msgs, err := rdb.XRevRangeN(ctx, key, "+", "-", 50).Result()
	if err != nil {
		return false
	}
	for _, m := range msgs {
		line, _ := m.Values["line"].(string)
		l := strings.ToLower(line)
		if strings.Contains(l, "restart") || strings.Contains(l, "startup script") {
			return true
		}
	}
	return false
}
