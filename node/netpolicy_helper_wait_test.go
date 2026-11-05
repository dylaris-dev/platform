package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// The helper is AutoRemove and runs nft for a few milliseconds. A wait sent
// after the start can reach the daemon once the helper is already gone, which
// answers 404 and read as "could not be given their rules" although they were
// applied. The daemon here behaves that way: a wait that arrives after the
// start finds no container; one registered before it gets the exit code.
func TestPolicyHelperWaitIsRegisteredBeforeTheStart(t *testing.T) {
	var once sync.Once
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/containers/create"):
			w.Write([]byte(`{"Id":"helper-id"}`))
		case strings.HasSuffix(p, "/start"):
			once.Do(func() { close(started) })
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(p, "/wait"):
			select {
			case <-started:
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"No such container: helper-id"}`))
				return
			default:
			}
			// Registered while it exists: the daemon answers the header at
			// once and the exit code when the helper exits.
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
			}
			w.Write([]byte(`{"StatusCode":0}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	dm := &DockerManager{cli: cli, ctx: t.Context()}

	if err := dm.applyRulesetWith(t.Context(), "img:1", "host", "flush ruleset"); err != nil {
		t.Fatalf("a helper that applied its rules and exited 0 reported: %v", err)
	}
}
