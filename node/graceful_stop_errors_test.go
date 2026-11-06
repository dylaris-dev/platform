package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"
)

// gracefulStop's wait loop read ANY inspect error as "stopped": a daemon that
// failed one inspect mid-wait made it return nil for a server still running,
// and the caller copied, deleted or reinstalled under the live JVM. Only "no
// such container" or a container that is not running means stopped; any other
// error keeps waiting and ends in the forced stop, whose error comes back.
func TestGracefulStopWaitLoopOnlyTrustsAnAnswer(t *testing.T) {
	prevWait := gracefulStopWait
	gracefulStopWait = 300 * time.Millisecond
	t.Cleanup(func() { gracefulStopWait = prevWait })

	const uuid = "cdcdcdcd-2222-3333-4444-555555555555"
	for _, c := range []struct {
		name    string
		later   int    // HTTP status of every inspect after the first
		body    string // its body when 200
		stopErr bool   // the forced docker stop fails
		wantErr bool
	}{
		{name: "exited", later: 200, body: `{"Id":"c","State":{"Running":false}}`},
		{name: "gone", later: 404, body: `{"message":"No such container"}`},
		{name: "daemon error, forced stop fails", later: 500, body: `{"message":"daemon busy"}`, stopErr: true, wantErr: true},
		{name: "daemon error, forced stop works", later: 500, body: `{"message":"daemon busy"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var inspects atomic.Int32
			var stops atomic.Int32
			started := time.Now().Format(time.RFC3339Nano) // just started: no 3s save-all
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/mc_"+uuid+"/json"):
					if inspects.Add(1) == 1 {
						w.Write([]byte(`{"Id":"c","State":{"Running":true,"StartedAt":"` + started + `"}}`))
						return
					}
					w.WriteHeader(c.later)
					w.Write([]byte(c.body))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
					stops.Add(1)
					if c.stopErr {
						w.WriteHeader(http.StatusInternalServerError)
						w.Write([]byte(`{"message":"cannot stop"}`))
						return
					}
					w.WriteHeader(http.StatusNoContent)
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
			dm := &DockerManager{cli: cli, ctx: t.Context(), localDataPath: t.TempDir()}
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

			err = gracefulStop(rdb, uuid, dm)
			if (err != nil) != c.wantErr {
				t.Fatalf("gracefulStop error = %v, want error %v", err, c.wantErr)
			}
			if wantStop := c.later == 500; (stops.Load() > 0) != wantStop {
				t.Fatalf("forced stop sent = %v, want %v", stops.Load() > 0, wantStop)
			}
		})
	}
}

// Every command that goes on to touch a server's files or recreate it must
// abort when the stop fails, not carry on under a JVM that is still running.
// This reads the source, as TestDestructiveCommandsHoldTheBusyInterlock does,
// because these handlers need real storage and Docker to drive.
func TestFileWorkAbortsOnAFailedStop(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"setup", "restart", "reinstall", "migrate_storage"} {
		m := regexp.MustCompile(`(?s)case "` + action + `":(.*?)\n\tcase "`).FindSubmatch(src)
		if m == nil {
			t.Fatalf("case %q not found in the command switch", action)
		}
		body := string(m[1])
		if !strings.Contains(body, "err := gracefulStop(") {
			t.Errorf("case %q does not check gracefulStop's error", action)
		}
		if regexp.MustCompile(`(?m)^\s*(gracefulStop\(|dm\.PowerAction\([^)]*"stop"\))`).MatchString(body) {
			t.Errorf("case %q still stops the server with the error dropped", action)
		}
	}
	rs, err := os.ReadFile("backup_restore.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rs), "err := gracefulStop(") || regexp.MustCompile(`(?m)^\s*gracefulStop\(`).Match(rs) {
		t.Error("RunRestore does not check gracefulStop's error")
	}
}
