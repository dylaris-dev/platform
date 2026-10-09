package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/docker/docker/client"
)

// noContainerDocker answers every container inspect with 404 and counts what
// would put a container on the host.
func noContainerDocker(t *testing.T, dataPath string) (*DockerManager, *atomic.Int32) {
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
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"No such container"}`))
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
	return &DockerManager{cli: cli, ctx: t.Context(), localDataPath: dataPath}, &created
}

// A server with no container (stopped after a move, a sub-server delete or a
// storage move) was created AND started by a resource change, whatever its
// state: a routing-mode switch sends one to every server, so suspended and
// stopped ones came up.
func TestUpdateResourcesStartsNoServerThatHasNoContainer(t *testing.T) {
	const uuid = "12121212-2222-3333-4444-555555555555"
	dm, created := noContainerDocker(t, t.TempDir())
	cfg := ServerConfig{UUID: uuid, ActiveSubServer: "survival"}
	cfg.Docker.Image = "img:1"
	cfg.Docker.RAM = 2048
	cfg.Docker.Command = "java -jar server.jar nogui"
	if _, err := dm.UpdateResources(cfg); err != nil {
		t.Fatalf("UpdateResources: %v", err)
	}
	if n := created.Load(); n != 0 {
		t.Fatalf("a server without a container got %d create/start call(s)", n)
	}
}

// The change lands in the saved config the next start builds from, and only
// what a resource change carries replaces a saved value.
func TestUpdateResourcesWithoutAContainerKeepsTheSavedConfigsGoodParts(t *testing.T) {
	const uuid = "13131313-2222-3333-4444-555555555555"
	data := t.TempDir()
	dm, _ := noContainerDocker(t, data)
	dir := filepath.Join(data, "servers", uuid)
	if err := os.MkdirAll(filepath.Join(dir, "survival"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "survival", "server.jar"), []byte("jar"), 0o644)
	saved := ServerConfig{UUID: uuid, ActiveSubServer: "survival"}
	saved.Docker.Image = "img:saved"
	saved.Docker.RAM = 1024
	saved.Docker.HostPort = 25601
	saved.Docker.Command = "java -Xms1024M -Xmx1024M -Dkeep=me -jar server.jar nogui"
	b, _ := json.Marshal(saved)
	os.WriteFile(filepath.Join(dir, ".node_config.json"), b, 0o644)

	// What a routing switch sends after the active sub-server's name was lost:
	// no sub-server, no port, Core's jar-form command.
	change := ServerConfig{UUID: uuid}
	change.Docker.RAM = 4096
	change.Docker.CPULimit = 2
	change.Docker.Command = "java -jar server.jar nogui"
	got, err := dm.UpdateResources(change)
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveSubServer != "survival" || got.Docker.HostPort != 25601 || got.Docker.Image != "img:saved" {
		t.Errorf("saved values lost: sub %q port %d image %q", got.ActiveSubServer, got.Docker.HostPort, got.Docker.Image)
	}
	if got.Docker.RAM != 4096 || got.Docker.CPULimit != 2 {
		t.Errorf("resources not applied: ram %d cpu %v", got.Docker.RAM, got.Docker.CPULimit)
	}
	if !strings.Contains(got.Docker.Command, "-Xmx3482M") || !strings.Contains(got.Docker.Command, "-Dkeep=me") {
		t.Errorf("command not rebuilt from disk with the new memory and the saved flags: %q", got.Docker.Command)
	}
}

// The handler saves what UpdateResources applied, not the payload.
func TestUpdateResourcesHandlerSavesTheAppliedConfig(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i, j := strings.Index(s, "\tcase \"update_resources\":"), strings.Index(s, "\tcase \"delete\":")
	if i < 0 || j < i {
		t.Fatal("the update_resources case moved; move this assertion with it")
	}
	body := s[i:j]
	if strings.Contains(body, "saveNodeConfig(resServerPath, cmd.Config)") || !strings.Contains(body, "saveNodeConfig(resServerPath, applied)") {
		t.Error("update_resources saves the payload instead of the config it applied")
	}
}

// A saved sub-server that is not one plain name is not used to find the jar.
func TestUpdateResourcesIgnoresAPlantedSavedSubServer(t *testing.T) {
	const uuid = "14141414-2222-3333-4444-555555555555"
	data := t.TempDir()
	dm, _ := noContainerDocker(t, data)
	dir := filepath.Join(data, "servers", uuid)
	os.MkdirAll(dir, 0o755)
	saved := ServerConfig{UUID: uuid, ActiveSubServer: "../other/survival"}
	saved.Docker.Image = "img:1"
	b, _ := json.Marshal(saved)
	os.WriteFile(filepath.Join(dir, ".node_config.json"), b, 0o644)

	change := ServerConfig{UUID: uuid}
	change.Docker.RAM = 2048
	got, err := dm.UpdateResources(change)
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveSubServer != "" {
		t.Errorf("planted sub-server %q survived the merge", got.ActiveSubServer)
	}
}
