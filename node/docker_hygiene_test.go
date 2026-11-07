package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// A size cap only for drivers that take one: any other driver may refuse the
// options, and the server's container would not be created at all.
func TestBoundedLogConfig(t *testing.T) {
	for _, d := range []string{"json-file", "local"} {
		if c := boundedLogConfig(d); c.Type != d || c.Config["max-size"] == "" {
			t.Errorf("%s: %+v, want a size cap", d, c)
		}
	}
	for _, d := range []string{"", "journald", "syslog", "fluentd", "none"} {
		if c := boundedLogConfig(d); c.Type != "" || c.Config != nil {
			t.Errorf("%q: %+v, want the daemon's default left alone", d, c)
		}
	}
}

// Only our Minecraft runtime images that nothing tags any more, or the retired
// repository's, are removed - never a customer's own image.
func TestStaleRuntimeImages(t *testing.T) {
	tests := []struct {
		name string
		img  dockerimage.Summary
		want bool
	}{
		{"previous build", dockerimage.Summary{RepoDigests: []string{"ghcr.io/dylaris-dev/platform-mc-java21@sha256:aa"}}, true},
		{"current build", dockerimage.Summary{RepoTags: []string{"ghcr.io/dylaris-dev/platform-mc-java21:latest"}}, false},
		{"retired repository", dockerimage.Summary{RepoTags: []string{"ghcr.io/bartis-dev/dylaris-platform-mc-java21:latest"}}, true},
		{"our node image", dockerimage.Summary{RepoDigests: []string{"ghcr.io/dylaris-dev/platform-node@sha256:bb"}}, false},
		{"someone else's", dockerimage.Summary{RepoDigests: []string{"docker.io/library/postgres@sha256:cc"}}, false},
		{"retired previous build", dockerimage.Summary{RepoDigests: []string{"ghcr.io/bartis-dev/dylaris-platform-mc-java17@sha256:dd"}}, true},
		{"untagged without digest", dockerimage.Summary{}, false},
		{"retired and current tag", dockerimage.Summary{RepoTags: []string{"ghcr.io/bartis-dev/dylaris-platform-mc-java21:latest", "ghcr.io/dylaris-dev/platform-mc-java21:latest"}}, false},
	}
	for _, tt := range tests {
		if got := isStaleRuntimeImage(tt.img); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}

// Pinned in the source: these need a Docker daemon.
func TestDockerHygieneIsWired(t *testing.T) {
	read := func(f string) string {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	dm := read("docker_mgr.go")
	if !strings.Contains(dm, "hc.LogConfig = currentLogConfig()") {
		t.Error("tenant containers are no longer log-capped")
	}
	if !strings.Contains(dm, "Labels:       map[string]string{roleLabel: roleInstaller, ownerLabel: nodeIdentity(nodeSecretDir)},") {
		t.Error("installer containers are no longer labelled, so a restart's leftovers cannot be found")
	}
	if !strings.Contains(read("main.go"), "dockerMgr.startDockerHygiene(ctx)") {
		t.Error("the docker hygiene is not started")
	}
	h := read("docker_hygiene.go")
	if strings.Contains(h, "Force: true") {
		t.Error("hygiene removes by force; an image or container in use must stay")
	}
}

// R04: an installer still running when the agent restarts belongs to a process
// that is gone, and the redelivered setup runs a new one in the same directory.
// The startup sweep stops it and removes it, before any command is taken.
func TestRemoveOrphanInstallersStopsRunningOnes(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/containers/json"):
			w.Write([]byte(`[{"Id":"run1","State":"running"},{"Id":"done1","State":"exited"},{"Id":"new1","State":"created"}]`))
		case strings.Contains(p, "/containers/"):
			parts := strings.Split(p[strings.Index(p, "/containers/")+len("/containers/"):], "/")
			verb := "remove"
			if len(parts) > 1 {
				verb = parts[1]
			}
			mu.Lock()
			got = append(got, verb+" "+parts[0])
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	dm := &DockerManager{cli: cli, ctx: t.Context()}
	dm.removeOrphanInstallers(t.Context())
	want := []string{"stop run1", "remove run1", "remove done1", "remove new1"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
