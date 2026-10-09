package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/client"
)

// installerDocker is a Docker API that records every container create in
// order and answers the rest of what installerNetwork asks.
type installerDocker struct {
	mu      sync.Mutex
	creates []map[string]any
	removed []string
}

func (d *installerDocker) manager(t *testing.T, policyImage string) *DockerManager {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case r.Method == "GET" && strings.HasSuffix(p, "/networks"):
			w.Write([]byte(`[{"Id":"0123456789abcdef0123","Name":"dylaris_net"}]`))
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/create"):
			body, _ := io.ReadAll(r.Body)
			var m map[string]any
			json.Unmarshal(body, &m)
			d.mu.Lock()
			d.creates = append(d.creates, m)
			id := fmt.Sprintf("c%d", len(d.creates))
			d.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"Id":"` + id + `"}`))
		case r.Method == "POST" && strings.HasSuffix(p, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "POST" && strings.HasSuffix(p, "/wait"):
			w.Write([]byte(`{"StatusCode":0}`))
		case r.Method == "DELETE":
			d.mu.Lock()
			d.removed = append(d.removed, p[strings.LastIndex(p, "/")+1:])
			d.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"unexpected ` + r.Method + ` ` + p + `"}`))
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return &DockerManager{cli: cli, ctx: t.Context(), netPolicyImage: policyImage}
}

var testEgress = func() (*netEgress, error) {
	return &netEgress{overlay: "10.10.0.0/16", redisIP: "10.0.0.10", redisPort: 6379, gamePort: 25565}, nil
}

// On a node that enforces the policy the installer shares the namespace of a
// holder container the egress rules were applied to before it starts.
func TestInstallerRunsBehindTheEgressRules(t *testing.T) {
	d := &installerDocker{}
	dm := d.manager(t, "node-image")
	mode, nc, release := dm.installerNetwork(context.Background(), testEgress)
	if string(mode) != "container:c1" || nc != nil {
		t.Fatalf("installer network = %q, %v; want the holder's namespace", mode, nc)
	}
	if len(d.creates) != 2 {
		t.Fatalf("%d containers created, want holder + policy helper", len(d.creates))
	}
	holderNets, _ := json.Marshal(d.creates[0]["NetworkingConfig"])
	if !strings.Contains(string(holderNets), "dylaris_net") {
		t.Errorf("holder is not on the shared network: %s", holderNets)
	}
	if holderHC, _ := d.creates[0]["HostConfig"].(map[string]any); holderHC["AutoRemove"] != true {
		t.Error("the holder does not remove itself; a node that dies mid-install leaves it behind")
	}
	helperHC, _ := d.creates[1]["HostConfig"].(map[string]any)
	if helperHC["NetworkMode"] != "container:c1" {
		t.Errorf("policy helper joined %v, want the holder", helperHC["NetworkMode"])
	}
	cmd, _ := json.Marshal(d.creates[1]["Cmd"])
	if !strings.Contains(string(cmd), "169.254.0.0/16 drop") {
		t.Errorf("the rules applied carry no egress chain: %s", cmd)
	}
	release()
	if len(d.removed) != 1 || d.removed[0] != "c1" {
		t.Errorf("holder not removed after the install: %v", d.removed)
	}
}

// Where the node enforces no policy, or the rules cannot be worked out, the
// installer still joins the shared network instead of the default bridge.
func TestInstallerJoinsTheSharedNetworkWithoutPolicy(t *testing.T) {
	for name, c := range map[string]struct {
		image  string
		egress func() (*netEgress, error)
	}{
		"no policy on this node": {"", testEgress},
		"egress unknown":         {"node-image", func() (*netEgress, error) { return nil, errors.New("no subnet") }},
	} {
		d := &installerDocker{}
		mode, nc, release := d.manager(t, c.image).installerNetwork(context.Background(), c.egress)
		release()
		nets, _ := json.Marshal(nc)
		if mode != "" || !strings.Contains(string(nets), "dylaris_net") {
			t.Errorf("%s: mode %q, networks %s; want the shared network", name, mode, nets)
		}
		if len(d.creates) != 0 {
			t.Errorf("%s: %d containers created, want none", name, len(d.creates))
		}
	}
}

// The installer runs with a CPU cap no larger than the host has.
func TestInstallerHasACPUCap(t *testing.T) {
	if n := installerNanoCPUs(); n <= 0 || n > 2e9 {
		t.Fatalf("installer NanoCPUs = %d, want 1-2 CPUs", n)
	}
}

// RunInstallerContainer creates the installer with what installerNetwork
// decided and with the CPU cap.
func TestRunInstallerContainerUsesTheInstallerNetwork(t *testing.T) {
	b, err := os.ReadFile("docker_mgr.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	i := strings.Index(s, "func (dm *DockerManager) RunInstallerContainer(")
	j := strings.Index(s[i:], "\n}\n")
	body := s[i : i+j]
	for _, want := range []string{
		"if dm.netPolicyImage != \"\" {\n\t\thc.Resources.NanoCPUs = installerNanoCPUs()\n\t}",
		"netMode, nc, releaseNet := dm.installerNetwork(ctx, dm.egressPolicy)",
		"defer releaseNet()",
		"hc.NetworkMode = netMode",
		`dm.cli.ContainerCreate(ctx, cc, hc, nc, nil, "")`,
		// A server pack's own installer runs here: its output is bounded.
		"Tail:       installerLogTail,",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("RunInstallerContainer lost %q", want)
		}
	}
}

// An installer the tenant shipped (a server pack's) that never exits held the
// server in "installing" with 2 GiB reserved, for good.
func TestRunJavaInstallerIsBounded(t *testing.T) {
	b, err := os.ReadFile("installer.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	i := strings.Index(s, "func runJavaInstaller(")
	j := strings.Index(s[i:], "\n}\n")
	body := s[i : i+j]
	for _, want := range []string{
		"ctx, cancel := context.WithTimeout(context.Background(), installerTimeout)",
		"dockerManager.RunInstallerContainer(ctx, ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("runJavaInstaller lost %q", want)
		}
	}
}
