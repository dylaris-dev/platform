package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

// networkDocker lists the given networks and records the name of any create.
func networkDocker(t *testing.T, list string, created *string) *DockerManager {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/networks"):
			w.Write([]byte(list))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/networks/create"):
			body, _ := io.ReadAll(r.Body)
			var m struct{ Name string }
			json.Unmarshal(body, &m)
			*created = m.Name
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"Id":"feedfacefeedfacefeed"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return &DockerManager{cli: cli, ctx: t.Context(), selfHostNet: true}
}

const oldAndKitNets = `[{"Id":"aaaaaaaaaaaaaaaaaaaa","Name":"dylaris_net"},{"Id":"bbbbbbbbbbbbbbbbbbbb","Name":"byon_dylaris_net"}]`

// A machine that ran an older node still has the dylaris_net that node made.
// The kit names its own network; servers must land on that one, where the
// link is, and not on the leftover.
func TestTheKitsNetworkWinsOverALeftover(t *testing.T) {
	t.Setenv("NODE_DOCKER_NETWORK", "byon_dylaris_net")
	var created string
	_, name, err := networkDocker(t, oldAndKitNets, &created).ensureGlobalNetwork()
	if err != nil || name != "byon_dylaris_net" || created != "" {
		t.Fatalf("got %q, %v, created %q; want byon_dylaris_net", name, err, created)
	}
}

// Without the link beside it nothing else creates the kit's network: the node
// makes it under the kit's name, not dylaris_net.
func TestTheKitsNetworkIsCreatedUnderItsName(t *testing.T) {
	t.Setenv("NODE_DOCKER_NETWORK", "external_dylaris_net")
	var created string
	_, name, err := networkDocker(t, `[{"Id":"aaaaaaaaaaaaaaaaaaaa","Name":"dylaris_net"}]`, &created).ensureGlobalNetwork()
	if err != nil || created != "external_dylaris_net" || name != "external_dylaris_net" {
		t.Fatalf("got %q, %v, created %q; want external_dylaris_net", name, err, created)
	}
}

// No kit name: the fleet's prefixed overlay is still found as before.
func TestWithoutAKitNameAnyDylarisNetIsFound(t *testing.T) {
	t.Setenv("NODE_DOCKER_NETWORK", "")
	var created string
	_, name, err := networkDocker(t, `[{"Id":"cccccccccccccccccccc","Name":"platform_dylaris_net"}]`, &created).ensureGlobalNetwork()
	if err != nil || name != "platform_dylaris_net" || created != "" {
		t.Fatalf("got %q, %v, created %q", name, err, created)
	}
}
