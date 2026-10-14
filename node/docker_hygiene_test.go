package main

import (
	"os"
	"strings"
	"testing"

	dockerimage "github.com/docker/docker/api/types/image"
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
		{"previous build", dockerimage.Summary{RepoDigests: []string{"ghcr.io/dylaris-dev/mc-java21@sha256:aa"}}, true},
		{"current build", dockerimage.Summary{RepoTags: []string{"ghcr.io/dylaris-dev/mc-java21:latest"}}, false},
		{"retired repository", dockerimage.Summary{RepoTags: []string{"ghcr.io/bartis-dev/dylaris-mc-java21:latest"}}, true},
		{"our node image", dockerimage.Summary{RepoDigests: []string{"ghcr.io/dylaris-dev/platform-node@sha256:bb"}}, false},
		{"someone else's", dockerimage.Summary{RepoDigests: []string{"docker.io/library/postgres@sha256:cc"}}, false},
		{"untagged without digest", dockerimage.Summary{}, false},
		{"retired and current tag", dockerimage.Summary{RepoTags: []string{"ghcr.io/bartis-dev/dylaris-mc-java21:latest", "ghcr.io/dylaris-dev/mc-java21:latest"}}, false},
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
