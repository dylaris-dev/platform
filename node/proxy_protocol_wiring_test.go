package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestBungeeExpectsProxy(t *testing.T) {
	// BungeeCord's own save of the node's listener.
	saved := "connection_throttle: 4000\nlisteners:\n- query_port: 25577\n  motd: '&1Just another BungeeCord - Forced Host'\n  priorities:\n  - lobby\n  forced_hosts:\n    pvp.md-5.net: pvp\n  host: 0.0.0.0:25565\n  proxy_protocol: true\n  max_players: 1\nservers:\n  lobby:\n    address: localhost:25565\n"
	cases := map[string]bool{
		saved: true,
		"listeners:\n- host: 0.0.0.0:25565\n  proxy_protocol: true\n- host: 0.0.0.0:25566\n  proxy_protocol: true\n": true,
		"listeners:\n- host: 0.0.0.0:25565\n  proxy_protocol: true\n- host: 0.0.0.0:25566\n":                         false,
		"listeners:\n- host: 0.0.0.0:25565\n  proxy_protocol: false\n":                                               false,
		"listeners:\n- host: 0.0.0.0:25565\nother:\n  proxy_protocol: true\n":                                        false,
		"": false,
	}
	for in, want := range cases {
		if got := bungeeExpectsProxy([]byte(in)); got != want {
			t.Errorf("%q = %v, want %v", in, got, want)
		}
	}
}

// The answer survives a cancelled command context (a node being stopped) and
// lands in the durable key as well as on the channel.
func TestReportProxyProtocolIsDurableAndDetached(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reportProxyProtocol(ctx, rdb, "srv-1", "main", false)
	if v, err := mr.Get("dylaris:server:srv-1:proxy_protocol_result"); err != nil || v != "0" {
		t.Fatalf("result = %q, %v", v, err)
	}
	if ttl := mr.TTL("dylaris:server:srv-1:proxy_protocol_result"); ttl <= 0 {
		t.Fatalf("result key has no expiry (%v)", ttl)
	}
	reportProxyProtocol(ctx, rdb, "srv-1", "main", true)
	if v, _ := mr.Get("dylaris:server:srv-1:proxy_protocol_result"); v != "1" {
		t.Fatalf("result = %q", v)
	}
}

func TestProxyProtocolReporterAnswersOnce(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	r := proxyProtocolReporter(context.Background(), rdb, InstallerConfig{ProxyProtocol: true}, "srv-1", "main")
	r(true)
	r(false) // the deferred no after a yes
	if v, _ := mr.Get("dylaris:server:srv-1:proxy_protocol_result"); v != "1" {
		t.Fatalf("result = %q, want the first answer", v)
	}
	r2 := proxyProtocolReporter(context.Background(), rdb, InstallerConfig{}, "srv-2", "main")
	r2(false)
	if mr.Exists("dylaris:server:srv-2:proxy_protocol_result") {
		t.Fatal("answered an install that carried no request")
	}
}

// processCommand's setup answers no on every way out that never reaches the
// write, so Core can switch the server back off.
func TestSetupAnswersNoOnEveryEarlyReturn(t *testing.T) {
	cases := map[string]struct {
		installer InstallerConfig
		prepare   func(t *testing.T, subDir string)
		deleted   bool
	}{
		"install failed":   {installer: InstallerConfig{Type: "not-an-installer", ProxyProtocol: true}},
		"no start command": {installer: InstallerConfig{Type: "upload", ProxyProtocol: true}},
		"eula refused": {
			installer: InstallerConfig{Type: "upload", ProxyProtocol: true},
			prepare: func(t *testing.T, subDir string) {
				os.MkdirAll(subDir, 0o755)
				if err := os.Symlink(t.TempDir(), filepath.Join(subDir, "eula.txt")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
		"server deleted meanwhile": {installer: InstallerConfig{Type: "upload", ProxyProtocol: true}, deleted: true},
	}
	uuids := map[string]string{
		"install failed":           "6f1c2b3a-0000-4000-8000-000000000001",
		"no start command":         "6f1c2b3a-0000-4000-8000-000000000002",
		"eula refused":             "6f1c2b3a-0000-4000-8000-000000000003",
		"server deleted meanwhile": "6f1c2b3a-0000-4000-8000-000000000004",
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			storage := NewStorageManager(t.TempDir(), rdb)
			uuid := uuids[name]
			if c.prepare != nil {
				c.prepare(t, filepath.Join(storage.GetServerDir(uuid), "main"))
			}
			if c.deleted {
				markServerDeleted(uuid)
			}
			cmd := NodeCommand{Action: "setup", Installer: c.installer}
			cmd.Config.UUID = uuid
			cmd.Config.ActiveSubServer = "main"
			processCommand(context.Background(), cmd, "", rdb, nil, "node-1", nil, storage)
			if v, err := mr.Get("dylaris:server:" + uuid + ":proxy_protocol_result"); err != nil || v != "0" {
				t.Fatalf("result = %q, %v; want 0", v, err)
			}
		})
	}
	t.Run("no request, no answer", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		storage := NewStorageManager(t.TempDir(), rdb)
		cmd := NodeCommand{Action: "setup", Installer: InstallerConfig{Type: "not-an-installer"}}
		cmd.Config.UUID = "6f1c2b3a-0000-4000-8000-000000000005"
		cmd.Config.ActiveSubServer = "main"
		processCommand(context.Background(), cmd, "", rdb, nil, "node-1", nil, storage)
		if mr.Exists("dylaris:server:6f1c2b3a-0000-4000-8000-000000000005:proxy_protocol_result") {
			t.Fatal("answered a setup that carried no request")
		}
	})
}
