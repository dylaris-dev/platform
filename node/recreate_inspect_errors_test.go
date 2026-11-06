package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"
)

// inspectStatusDocker answers every container inspect with the given status
// and counts what would put a container on the host.
func inspectStatusDocker(t *testing.T, status int) (*DockerManager, *atomic.Int32) {
	t.Helper()
	var created atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/containers/create"), strings.HasSuffix(p, "/start"):
			created.Add(1)
			w.Write([]byte(`{"Id":"new-id-0123456789ab"}`))
		case strings.Contains(p, "/containers/") && strings.HasSuffix(p, "/json"):
			w.WriteHeader(status)
			w.Write([]byte(`{"message":"inspect answer"}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return &DockerManager{cli: cli, ctx: t.Context(), localDataPath: t.TempDir()}, &created
}

// Only "no such container" means there is no container to change. Any other
// inspect failure (daemon busy, timeout, a 500) was read the same way: the
// change was reported applied while the running container kept its old
// settings, and nothing retried it.
func TestRecreatePathsOnlyTreatNotFoundAsNoContainer(t *testing.T) {
	const uuid = "34343434-2222-3333-4444-555555555555"
	for _, c := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "no such container", status: http.StatusNotFound},
		{name: "daemon error", status: http.StatusInternalServerError, wantErr: true},
	} {
		cfg := ServerConfig{UUID: uuid, ActiveSubServer: "survival"}
		cfg.Docker.Image = "img:1"
		cfg.Docker.RAM = 2048
		cfg.Docker.Command = "java -jar server.jar nogui"

		t.Run("UpdateResources/"+c.name, func(t *testing.T) {
			dm, created := inspectStatusDocker(t, c.status)
			if _, err := dm.UpdateResources(cfg); (err != nil) != c.wantErr {
				t.Fatalf("UpdateResources error = %v, want error %v", err, c.wantErr)
			}
			if n := created.Load(); n != 0 {
				t.Fatalf("got %d create/start call(s)", n)
			}
		})
		t.Run("RecreateKeepingRunState/"+c.name, func(t *testing.T) {
			dm, created := inspectStatusDocker(t, c.status)
			if err := dm.RecreateKeepingRunState(cfg); (err != nil) != c.wantErr {
				t.Fatalf("RecreateKeepingRunState error = %v, want error %v", err, c.wantErr)
			}
			if n := created.Load(); n != 0 {
				t.Fatalf("got %d create/start call(s)", n)
			}
		})
	}
}

// A stop whose console commands nobody read (the log-shipper could not reach
// Redis) left "save-all" and "stop" in :input. The recreated container's
// shipper then fed them to the booting JVM, which stopped itself and was held
// down as stopped_by_server. Once the container is down, whatever is still
// queued was meant for the JVM that just exited.
func TestGracefulStopDropsUnreadConsoleInput(t *testing.T) {
	prevWait := gracefulStopWait
	gracefulStopWait = 300 * time.Millisecond
	t.Cleanup(func() { gracefulStopWait = prevWait })

	const uuid = "45454545-2222-3333-4444-555555555555"
	for _, c := range []struct {
		name  string
		later int // HTTP status of every inspect after the first
		body  string
		want  bool // :input still exists afterwards
	}{
		{name: "exited", later: 200, body: `{"Id":"c","State":{"Running":false}}`},
		{name: "gone", later: 404, body: `{"message":"No such container"}`},
		// Not known to be down: the queued stop may still be on its way.
		{name: "still running", later: 200, body: `{"Id":"c","State":{"Running":true}}`, want: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var inspects atomic.Int32
			started := time.Now().Format(time.RFC3339Nano) // just started: no 3s save-all
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/mc_"+uuid+"/json") {
					if inspects.Add(1) == 1 {
						w.Write([]byte(`{"Id":"c","State":{"Running":true,"StartedAt":"` + started + `"}}`))
						return
					}
					w.WriteHeader(c.later)
					w.Write([]byte(c.body))
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(srv.Close)
			cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cli.Close() })
			dm := &DockerManager{cli: cli, ctx: t.Context(), localDataPath: t.TempDir()}
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

			if err := gracefulStop(rdb, uuid, dm); err != nil {
				t.Fatalf("gracefulStop: %v", err)
			}
			if got := mr.Exists("dylaris:server:" + uuid + ":input"); got != c.want {
				t.Fatalf(":input exists = %v, want %v", got, c.want)
			}
		})
	}
}
