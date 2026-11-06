package main

import (
	"context"
	"fmt"
	"testing"

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
		if got := logTailAsksRestart(ctx, rdb, key); got != c.want {
			t.Errorf("%q: asks restart = %v, want %v", c.line, got, c.want)
		}
	}
}
