package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Only vanilla "<Steve>" chat was skipped, so a player mentioning a restart in
// plugin-formatted chat, in a /msg or a /me brought back a server its owner
// stopped from the console within the next two minutes.
func TestPlayerTextDoesNotTurnAStopIntoARestart(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	ms := int64(2_000_000_000)
	for i, c := range []struct {
		line string
		want bool
	}{
		{"[12:00:00 INFO]: [Member] Steve: restart pls", false},
		{"[12:00:00 INFO]: [Discord] [Admin] Steve » can we restart?", false},
		{"[12:00:00 INFO]: Steve > restart", false},
		{"[12:00:00 INFO]: Steve issued server command: /msg Bob we need a restart", false},
		{"[12:00:00 INFO]: Steve issued server command: /me restart?", false},
		{"[12:00:00 INFO]: * Steve restart?", false},
		{"[12:00:00 INFO]: <Steve> restart pls", false},
		// Still a restart:
		{"[12:00:00 INFO]: Steve issued server command: /restart", true},
		{"[12:00:00 INFO]: Steve issued server command: /essentials:restart now", true},
		{"[12:00:00 INFO]: [AutoRestart] The server will restart in 10 seconds", true},
		{"[12:00:00 INFO]: [AutoRestart] Restart: in 10 seconds", true},
		{"[12:00:00 INFO]: Startup script './start.sh' does not exist! Stopping server.", true},
	} {
		key := serverLogStreamKey(fmt.Sprintf("chat-%d", i), "")
		ms++
		rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, ID: fmt.Sprintf("%d-0", ms), Values: map[string]interface{}{"line": c.line}})
		if got := logTailAsksRestart(ctx, rdb, key, time.Time{}); got != c.want {
			t.Errorf("%q: asks restart = %v, want %v", c.line, got, c.want)
		}
	}
}

// The log stream holds every run of a server, and the window was anchored only
// on the newest line: a console stop within two minutes of an in-game /restart
// read the previous run's /restart and the server came back against the stop.
func TestOnlyTheRunThatEndedCanAskForARestart(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	key := serverLogStreamKey("runs", "")
	ms := int64(3_000_000_000)
	add := func(line string) {
		rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, ID: fmt.Sprintf("%d-0", ms), Values: map[string]interface{}{"line": line}})
		ms += 1000
	}
	add("[12:00:00 INFO]: Steve issued server command: /restart")
	add("[12:00:01 INFO]: Startup script './start.sh' does not exist! Stopping server.")
	ms += 20_000
	runStart := time.UnixMilli(ms)
	add("[12:00:30 INFO]: Done (12.3s)! For help, type \"help\"")
	add("[12:01:00 INFO]: Stopping server")
	for _, c := range []struct {
		name  string
		since time.Time
		want  bool
	}{
		{"bounded by this run", runStart, false},
		{"run started before the restart", runStart.Add(-time.Minute), true},
		{"unknown start keeps the time window", time.Time{}, true},
	} {
		if got := logTailAsksRestart(ctx, rdb, key, c.since); got != c.want {
			t.Errorf("%s: asks restart = %v, want %v", c.name, got, c.want)
		}
	}
}

// A /say broadcast is "[Steve] text" ("[Server] text" from the console), and
// the "[Plugin] " tag stripping left only the text, so a say that mentioned a
// restart turned the console stop after it into a restart.
func TestASayBroadcastDoesNotTurnAStopIntoARestart(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	ms := int64(4_000_000_000)
	for i, c := range []struct {
		lines []string
		want  bool
	}{
		{[]string{"[12:00:00] [Server thread/INFO]: [Server] no restart tonight, shutting down"}, false},
		{[]string{"[12:00:00] [Server thread/INFO]: [Not Secure] [Steve] no restart tonight"}, false},
		{[]string{
			"[12:00:00 INFO]: Steve issued server command: /say no restart tonight",
			"[12:00:00 INFO]: [Steve] no restart tonight",
		}, false},
		{[]string{
			"[12:00:00 INFO]: Steve issued server command: /minecraft:say no restart tonight",
			"[12:00:00 INFO]: [Steve] no restart tonight",
		}, false},
		// A plugin's tag is not a say, even when someone else used /say.
		{[]string{
			"[12:00:00 INFO]: Steve issued server command: /say bye",
			"[12:00:00 INFO]: [Steve] bye",
			"[12:00:01 INFO]: [AutoRestart] The server will restart in 10 seconds",
		}, true},
	} {
		key := serverLogStreamKey(fmt.Sprintf("say-%d", i), "")
		for _, l := range c.lines {
			ms++
			rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, ID: fmt.Sprintf("%d-0", ms), Values: map[string]interface{}{"line": l}})
		}
		if got := logTailAsksRestart(ctx, rdb, key, time.Time{}); got != c.want {
			t.Errorf("case %d: asks restart = %v, want %v", i, got, c.want)
		}
	}
}
