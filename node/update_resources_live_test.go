package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/redis/go-redis/v9"
)

func TestJvmHeapMB(t *testing.T) {
	for _, tc := range []struct{ booked, heap int }{
		{512, 512}, // small plans keep the whole booking
		{1024, 1024},
		{2048, 2048},
		{2049, 2048}, // 2049-512, held at 2048
		{2560, 2048},
		{3072, 2560},
		{4096, 3482},   // 15% = 614
		{12288, 10445}, // 15% = 1843
		{20480, 18432}, // 15% = 3072, capped at 2048
	} {
		if got := jvmHeapMB(tc.booked); got != tc.heap {
			t.Errorf("jvmHeapMB(%d) = %d, want %d", tc.booked, got, tc.heap)
		}
	}
	for b, prev := 256, 0; b <= 32768; b++ {
		h := jvmHeapMB(b)
		if h < prev {
			t.Fatalf("jvmHeapMB(%d) = %d is below jvmHeapMB(%d) = %d", b, h, b-1, prev)
		}
		prev = h
	}
}

func TestNeedsRecreate(t *testing.T) {
	base := resourceShape{Memory: 4608 << 20, NanoCPUs: 2e9, Cpuset: "0-3", Image: "img:1", WorkDir: "/data/main", HostPort: 25601, ContainerPort: 25565}
	for _, tc := range []struct {
		name string
		edit func(*resourceShape)
		want bool
	}{
		{"nothing (disk-only change)", func(s *resourceShape) {}, false},
		{"cpu limit", func(s *resourceShape) { s.NanoCPUs = 4e9 }, false},
		{"cpuset", func(s *resourceShape) { s.Cpuset = "4-7" }, false},
		{"memory", func(s *resourceShape) { s.Memory = 8704 << 20 }, true},
		{"host port", func(s *resourceShape) { s.HostPort = 25650 }, true},
		{"container port", func(s *resourceShape) { s.ContainerPort = 25566 }, true},
		{"binding dropped (switch to gateway)", func(s *resourceShape) { s.HostPort, s.ContainerPort = 0, 0 }, true},
		{"image", func(s *resourceShape) { s.Image = "img:2" }, true},
		{"sub-server", func(s *resourceShape) { s.WorkDir = "/data/other" }, true},
		{"cpu limit removed (update ignores 0)", func(s *resourceShape) { s.NanoCPUs = 0 }, true},
		{"cpuset removed (update ignores empty)", func(s *resourceShape) { s.Cpuset = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := base
			tc.edit(&want)
			if got := needsRecreate(base, want); got != tc.want {
				t.Errorf("needsRecreate = %v, want %v", got, tc.want)
			}
		})
	}
	// Adding a limit or a pinning to a container that had none is live.
	if needsRecreate(resourceShape{}, resourceShape{NanoCPUs: 1e9, Cpuset: "0"}) {
		t.Error("adding a cpu limit and cpuset to an unlimited container asked for a recreate")
	}
}

func TestBoundPorts(t *testing.T) {
	pm := nat.PortMap{"25566/tcp": {{HostIP: "0.0.0.0", HostPort: "25601"}}}
	if h, c := boundPorts(pm); h != 25601 || c != 25566 {
		t.Errorf("boundPorts = %d, %d, want 25601, 25566", h, c)
	}
	if h, c := boundPorts(nil); h != 0 || c != 0 {
		t.Errorf("boundPorts(nil) = %d, %d, want 0, 0", h, c)
	}
}

// existingContainerDocker serves one running container mc_<uuid> with 4096 MB
// booked, 1 CPU, image img:1, sub-server main and host port 25601 -> 25566.
// It counts update, stop and create calls.
func existingContainerDocker(t *testing.T) (*DockerManager, map[string]*atomic.Int32) {
	t.Helper()
	// Direct port mode; other tests leave the mode globals set.
	r, f, pm, cp, iow, pids := getModes()
	setModes("ip_port", f, pm, 25565, iow, pids)
	t.Cleanup(func() { setModes(r, f, pm, cp, iow, pids) })
	calls := map[string]*atomic.Int32{"update": {}, "stop": {}, "create": {}}
	inspect := fmt.Sprintf(`{"Id":"abc","State":{"Running":true},
		"Config":{"Image":"img:1","WorkingDir":"/data/main","Cmd":["java","-jar","server.jar","nogui"]},
		"HostConfig":{"Memory":%d,"NanoCPUs":1000000000,"CpusetCpus":"",
			"PortBindings":{"25566/tcp":[{"HostIp":"0.0.0.0","HostPort":"25601"}]}}}`, int64(4096+512)<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/update"):
			calls["update"].Add(1)
			w.Write([]byte(`{"Warnings":[]}`))
		case strings.HasSuffix(p, "/stop"):
			calls["stop"].Add(1)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(p, "/containers/create"):
			calls["create"].Add(1)
			w.Write([]byte(`{"Id":"new-id-0123456789ab"}`))
		case strings.Contains(p, "/containers/mc_") && strings.HasSuffix(p, "/json"):
			w.Write([]byte(inspect))
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
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	dm := &DockerManager{cli: cli, ctx: t.Context(), localDataPath: t.TempDir()}
	dm.portMgr = NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	return dm, calls
}

func TestUpdateResourcesAppliesCPULiveAndCarriesPorts(t *testing.T) {
	const uuid = "31313131-2222-3333-4444-555555555555"
	dm, calls := existingContainerDocker(t)
	if err := dm.portMgr.SetPort(uuid, 25601); err != nil {
		t.Fatal(err)
	}
	cfg := ServerConfig{UUID: uuid}
	cfg.Docker.RAM, cfg.Docker.CPULimit, cfg.Docker.DiskLimit = 4096, 2, 10240
	got, err := dm.UpdateResources(cfg)
	if err != nil {
		t.Fatalf("UpdateResources: %v", err)
	}
	if calls["update"].Load() != 1 || calls["stop"].Load() != 0 || calls["create"].Load() != 0 {
		t.Errorf("cpu-only change: update=%d stop=%d create=%d, want a live update and no recreate",
			calls["update"].Load(), calls["stop"].Load(), calls["create"].Load())
	}
	if got.Docker.HostPort != 25601 || got.Docker.ContainerPort != 25566 {
		t.Errorf("saved ports %d -> %d, want the container's 25601 -> 25566", got.Docker.HostPort, got.Docker.ContainerPort)
	}
}

func TestUpdateResourcesRecreatesForMemory(t *testing.T) {
	const uuid = "32323232-2222-3333-4444-555555555555"
	dm, calls := existingContainerDocker(t)
	if err := dm.portMgr.SetPort(uuid, 25601); err != nil {
		t.Fatal(err)
	}
	cfg := ServerConfig{UUID: uuid}
	cfg.Docker.RAM, cfg.Docker.CPULimit = 8192, 1
	got, _ := dm.UpdateResources(cfg) // the recreate itself may fail against the fake
	if calls["stop"].Load() == 0 || calls["update"].Load() != 0 {
		t.Errorf("memory change: stop=%d update=%d, want a recreate", calls["stop"].Load(), calls["update"].Load())
	}
	if got.Docker.ContainerPort != 25566 {
		t.Errorf("container port %d, want the container's 25566 carried into the recreate", got.Docker.ContainerPort)
	}
}

func TestUpdateResourcesClaimsANewHostPortBeforeTheRecreate(t *testing.T) {
	const uuid = "33333333-2222-3333-4444-555555555555"
	dm, calls := existingContainerDocker(t)
	if err := dm.portMgr.SetPort(uuid, 25601); err != nil {
		t.Fatal(err)
	}
	if err := dm.portMgr.SetPort("someone-else", 25660); err != nil {
		t.Fatal(err)
	}
	cfg := ServerConfig{UUID: uuid}
	cfg.Docker.RAM, cfg.Docker.CPULimit = 4096, 1

	// Taken: refused while the old container is still there.
	cfg.Docker.HostPort = 25660
	if _, err := dm.UpdateResources(cfg); err == nil {
		t.Fatal("a host port another server holds was accepted")
	}
	if calls["stop"].Load() != 0 || dm.portMgr.GetPort(uuid) != 25601 {
		t.Fatalf("refused change still stopped the server (%d) or moved its port (%d)", calls["stop"].Load(), dm.portMgr.GetPort(uuid))
	}

	// Free: the allocation moves, so the recreate binds it.
	cfg.Docker.HostPort = 25650
	dm.UpdateResources(cfg)
	if got := dm.portMgr.GetPort(uuid); got != 25650 {
		t.Errorf("port manager holds %d, want the requested 25650", got)
	}
	if calls["stop"].Load() == 0 {
		t.Error("a host port change did not recreate the container")
	}
}
