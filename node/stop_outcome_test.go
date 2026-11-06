package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"
)

// A stop whose inspect failed (dockerd restarting under live-restore) or that
// found another node's container stopped nothing, and still reported the
// server "stopped".
func TestAStopThatStoppedNothingIsNotReportedStopped(t *testing.T) {
	const uuid = sharedDaemonUUID
	for _, c := range []struct {
		name string
		dm   func(t *testing.T) *DockerManager
		want string
	}{
		{"no such container", func(t *testing.T) *DockerManager { dm, _ := newSharedDaemon(t, "-", false, nil); return dm }, "stopped"},
		{"another node's container", func(t *testing.T) *DockerManager { dm, _ := newSharedDaemon(t, "node-b", true, nil); return dm }, ""},
		{"docker not answering", func(t *testing.T) *DockerManager {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"message":"daemon restarting"}`))
			}))
			t.Cleanup(srv.Close)
			cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cli.Close() })
			return &DockerManager{cli: cli, ctx: t.Context(), localDataPath: t.TempDir()}
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			dm := c.dm(t)
			storage := NewStorageManager(t.TempDir(), rdb)
			dm.storageMgr = storage
			cmd := NodeCommand{Action: "stop"}
			cmd.Config.UUID = uuid
			processCommand(context.Background(), cmd, "", rdb, dm, "node-a", nil, storage)
			if got, _ := rdb.Get(context.Background(), "dylaris:server:"+uuid+":status").Result(); got != c.want {
				t.Errorf("status %q, want %q", got, c.want)
			}
		})
	}
}
