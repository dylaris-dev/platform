package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"
)

// A start or restart from the panel rebuilt the container from its inspect but
// left the container port out, so it fell back to the node default: a server an
// admin had moved to 25570 (where a proxy also listens) came back bound to
// 25565 inside, unreachable from outside after every restart.
func TestRestartKeepsTheServersContainerPort(t *testing.T) {
	const uuid = "cdcdcdcd-2222-3333-4444-555555555555"
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
			w.Write([]byte(`{"Id":"c-id","State":{"Running":false},"Config":{"Image":"img:1","Labels":{}},` +
				`"HostConfig":{"PortBindings":{"25570/tcp":[{"HostIp":"0.0.0.0","HostPort":"25601"}]}},"NetworkSettings":{"Networks":{}}}`))
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

	if err := dm.RestartContainerInZone(uuid, "", 0); err != nil {
		t.Fatalf("restart: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(created) != 1 || created[0] != "25570/tcp" {
		t.Fatalf("restart published container port(s) %v, want [25570/tcp]", created)
	}
}
