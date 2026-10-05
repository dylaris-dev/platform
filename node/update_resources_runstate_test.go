package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/docker/docker/client"
)

// stoppedContainerDocker is a Docker API holding one STOPPED container
// mc_<uuid>, answering everything else permissively, and counting starts.
func stoppedContainerDocker(t *testing.T, uuid string) (*DockerManager, *atomic.Int32) {
	t.Helper()
	var starts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/start"):
			starts.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(p, "/containers/mc_"+uuid+"/json"), strings.HasSuffix(p, "/containers/new-id-0123456789ab/json"):
			w.Write([]byte(`{"Id":"old-id","State":{"Running":false},"Config":{"Image":"img:1","WorkingDir":"/data/main","Cmd":["java","-jar","server.jar"]},"HostConfig":{"Binds":["/srv:/data"]},"NetworkSettings":{"Networks":{"dylaris_net":{}}}}`))
		case strings.HasSuffix(p, "/containers/create"):
			w.Write([]byte(`{"Id":"new-id-0123456789ab"}`))
		case strings.Contains(p, "/networks") && r.Method == http.MethodGet && strings.HasSuffix(p, "/networks"):
			w.Write([]byte(`[{"Id":"0123456789abcdef0123","Name":"dylaris_net","IPAM":{"Config":[{"Subnet":"172.30.0.0/16","Gateway":"172.30.0.1"}]}}]`))
		case strings.Contains(p, "/networks/"):
			w.Write([]byte(`{"Id":"0123456789abcdef0123","Name":"dylaris_net","IPAM":{"Config":[{"Subnet":"172.30.0.0/16","Gateway":"172.30.0.1"}]}}`))
		case strings.HasSuffix(p, "/images/json"):
			w.Write([]byte(`[]`))
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
	return &DockerManager{cli: cli, ctx: t.Context()}, &starts
}

// A resource change used to end with the container running whatever it had
// been. A routing-mode switch sends one to every server, so every stopped
// server started - suspended tenants and full disks included.
func TestUpdateResourcesLeavesAStoppedServerStopped(t *testing.T) {
	const uuid = "11111111-2222-3333-4444-555555555555"
	dm, starts := stoppedContainerDocker(t, uuid)
	cfg := ServerConfig{UUID: uuid}
	cfg.Docker.Image = "img:1"
	cfg.Docker.RAM = 2048
	if _, err := dm.UpdateResources(cfg); err != nil {
		t.Fatalf("UpdateResources: %v", err)
	}
	if n := starts.Load(); n != 0 {
		t.Fatalf("a stopped server was started %d time(s) by a resource change", n)
	}
}

// A delete that lands while an install is running used to be undone by the
// install's last step, which created and started a container for a server
// Core no longer had.
func TestADeletedServerGetsNoContainerFromWorkStillRunning(t *testing.T) {
	const uuid = "99999999-2222-3333-4444-555555555555"
	dm, starts := stoppedContainerDocker(t, uuid)
	markServerDeleted(uuid)
	cfg := ServerConfig{UUID: uuid}
	cfg.Docker.Image = "img:1"
	if err := dm.RecreateWithCommand(cfg); err == nil {
		t.Fatal("a container was created for a deleted server")
	}
	if n := starts.Load(); n != 0 {
		t.Fatalf("a deleted server was started %d time(s)", n)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.jar"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !discardIfDeleted(uuid, dir) {
		t.Fatal("the install for a deleted server was not discarded")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the install's files were left behind: %v", err)
	}
	if discardIfDeleted("not-deleted", t.TempDir()) {
		t.Fatal("a live server's install was discarded")
	}
}
