package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestProxyInstallRefusal(t *testing.T) {
	mr := miniredis.RunT(t)
	st := &AppState{Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()})}
	mr.Set("dylaris:discovery:old-node", `{"releaseVersion":"2026.10.16.2"}`)
	mr.Set("dylaris:discovery:new-node", `{"releaseVersion":"`+proxyInstallSince+`"}`)
	mr.Set("dylaris:discovery:dev-node", `{"releaseVersion":""}`)

	cases := []struct {
		name, serverType, node, typ string
		want                        int
	}{
		{"velocity on a proxy, current node", "proxy", "new-node", "velocity", 0},
		{"waterfall on a proxy, current node", "proxy", "new-node", "waterfall", 0},
		{"bungeecord on a proxy, unstamped node", "proxy", "dev-node", "bungeecord", 0},
		{"bungeecord on a proxy, no heartbeat", "proxy", "unknown-node", "bungeecord", 0},
		{"velocity on a game server", "game", "new-node", "velocity", http.StatusBadRequest},
		{"bungeecord on an untyped server", "", "new-node", "bungeecord", http.StatusBadRequest},
		{"velocity to an older node", "proxy", "old-node", "velocity", http.StatusConflict},
		{"waterfall to an older node", "proxy", "old-node", "waterfall", http.StatusConflict},
		{"paper on a game server, older node", "game", "old-node", "paper", 0},
		{"paper on a proxy server", "proxy", "old-node", "paper", 0},
		{"upload on a proxy server", "proxy", "new-node", "upload", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg := proxyInstallRefusal(context.Background(), st, c.serverType, c.node, c.typ)
			if code != c.want {
				t.Fatalf("code = %d (%q), want %d", code, msg, c.want)
			}
			if (code == 0) != (msg == "") {
				t.Fatalf("code %d with message %q", code, msg)
			}
		})
	}
}
