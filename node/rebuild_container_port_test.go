package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"
)

// Setup, reinstall, switch_server, reconfigure and a version move all rebuild
// the container from Core's payload, which carries no container port. They
// bound the node default instead of the port an admin had set (saved in
// .node_config.json), so a server listening on that port was unreachable, and
// saveNodeConfig then wrote 0 over the saved port so it stayed lost.
func TestRebuildKeepsTheSavedContainerPort(t *testing.T) {
	const uuid = "abababab-2222-3333-4444-555555555555"
	r, f, p, cp, io, pids := getModes()
	t.Cleanup(func() { setModes(r, f, p, cp, io, pids) })
	setModes("ip_port", f, p, 25565, io, pids)

	var mu sync.Mutex
	var created []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/containers/mc_"+uuid+"/json"):
			// The old container already sits on the default: the bug's own aftermath.
			w.Write([]byte(`{"Id":"c-id","State":{"Running":false},"Config":{"Image":"img:1","Labels":{}},` +
				`"HostConfig":{"PortBindings":{"25565/tcp":[{"HostIp":"0.0.0.0","HostPort":"25601"}]}},"NetworkSettings":{"Networks":{}}}`))
		case strings.HasSuffix(p, "/containers/create"):
			var body struct {
				HostConfig container.HostConfig
			}
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			for port := range body.HostConfig.PortBindings {
				created = append(created, string(port))
			}
			mu.Unlock()
			w.Write([]byte(`{"Id":"new-id-0123456789ab"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/containers/new-id-0123456789ab/json"):
			w.Write([]byte(`{"Id":"new-id-0123456789ab","State":{"Running":true},"Config":{"Labels":{}},"NetworkSettings":{"Networks":{"dylaris_net":{}}}}`))
		case strings.HasSuffix(p, "/images/json"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/networks"):
			w.Write([]byte(`[{"Id":"0123456789abcdef0123","Name":"dylaris_net","IPAM":{"Config":[{"Subnet":"172.30.0.0/16","Gateway":"172.30.0.1"}]}}]`))
		case strings.Contains(p, "/networks/"):
			w.Write([]byte(`{"Id":"0123456789abcdef0123","Name":"dylaris_net","IPAM":{"Config":[{"Subnet":"172.30.0.0/16","Gateway":"172.30.0.1"}]}}`))
		case strings.Contains(p, "/containers/") && (r.Method == http.MethodDelete || r.Method == http.MethodPost):
			if strings.HasSuffix(p, "/wait") {
				w.Write([]byte(`{"StatusCode":0}`))
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
	mr := miniredis.RunT(t)
	dm := &DockerManager{cli: cli, ctx: t.Context(), localDataPath: t.TempDir()}
	dm.portMgr = NewPortManager(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "node-a", 25600, 25699, seqMode)
	if err := dm.portMgr.SetPort(uuid, 25601); err != nil {
		t.Fatal(err)
	}
	serverDir := dm.resolveLocalServerPath(uuid)
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	prev := dockerManager
	dockerManager = dm
	t.Cleanup(func() { dockerManager = prev })

	for _, tc := range []struct {
		name    string
		rebuild func(ServerConfig) error
		sent    int // the payload's containerPort
		want    string
	}{
		{"setup / switch_server / version move (started)", dm.RecreateWithCommand, 0, "25577/tcp"},
		{"setup / reinstall / version move (stopped)", dm.RecreateStopped, 0, "25577/tcp"},
		{"reconfigure", dm.RecreateKeepingRunState, 0, "25577/tcp"},
		{"a port Core does send wins", dm.RecreateWithCommand, 25590, "25590/tcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := ServerConfig{UUID: uuid}
			saved.Docker.ContainerPort = 25577
			data, _ := json.Marshal(saved)
			if err := os.WriteFile(filepath.Join(serverDir, ".node_config.json"), data, 0o644); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			created = nil
			mu.Unlock()

			cfg := ServerConfig{UUID: uuid, ActiveSubServer: "main"}
			cfg.Docker.Image = "img:2"
			cfg.Docker.ContainerPort = tc.sent
			if err := tc.rebuild(cfg); err != nil {
				t.Fatalf("rebuild: %v", err)
			}
			mu.Lock()
			got := append([]string(nil), created...)
			mu.Unlock()
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("rebuild published container port(s) %v, want [%s]", got, tc.want)
			}

			// What every one of those handlers does next.
			saveNodeConfig(serverDir, cfg)
			wantSaved := 25577
			if tc.sent != 0 {
				wantSaved = tc.sent
			}
			if got := dm.savedContainerPort(uuid); got != wantSaved {
				t.Fatalf("saved containerPort = %d after saveNodeConfig, want %d", got, wantSaved)
			}
		})
	}
}
