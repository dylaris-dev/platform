package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// newDirectFlow is newFlow on routing_mode ip_port, with the target reporting
// the given game port range and the route re-points counted.
func newDirectFlow(t *testing.T, targetRange string) (*MigrationOrchestrator, *flowStore, *simNodes, *int) {
	t.Helper()
	o, fs, sim, rdb := newFlow(t, "stopped")
	fs.mode = "ip_port"
	hb, _ := json.Marshal(NodeHeartbeat{ID: flowTargetToken, PortRange: targetRange})
	rdb.Set(context.Background(), "dylaris:discovery:"+flowTargetToken, hb, 0)
	routeMoves := 0
	o.gateway = flowGateway{routeMoves: &routeMoves}
	return o, fs, sim, &routeMoves
}

func sentHostPort(t *testing.T, sim *simNodes) (float64, bool) {
	t.Helper()
	got := sim.commands(flowTargetToken, "reconfigure")
	if len(got) != 1 {
		t.Fatalf("reconfigure sent %d times, want once", len(got))
	}
	d := got[0]["config"].(map[string]interface{})["docker"].(map[string]interface{})
	p, ok := d["hostPort"].(float64)
	return p, ok
}

// Without the gateway there is no route to re-point; the move itself is the
// same, and the server keeps its port when the target can give it the same one.
func TestADirectModeMoveKeepsAPortTheTargetCanGive(t *testing.T) {
	o, fs, sim, routeMoves := newDirectFlow(t, "25600-25699")
	fs.srv.HostPort = 25610
	fs.usedPorts = map[int][]int{2: {25600, 25611}}
	o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
	time.Sleep(100 * time.Millisecond)
	if len(fs.cutovers) != 1 || fs.cutovers[0] != 2 {
		t.Fatalf("server not moved to node 2: cutovers %v", fs.cutovers)
	}
	if *routeMoves != 0 {
		t.Fatalf("direct mode re-pointed a gateway route %d times", *routeMoves)
	}
	if p, ok := sentHostPort(t, sim); !ok || p != 25610 {
		t.Fatalf("hostPort sent %v (present %v), want 25610", p, ok)
	}
}

// A port outside the target's range would sit behind no port forward, and one
// already taken there would fail the container create: the target picks one.
func TestADirectModeMoveLetsTheTargetPickWhenThePortDoesNotFit(t *testing.T) {
	for name, tc := range map[string]struct {
		rng  string
		used []int
	}{
		"outside the target range": {rng: "25700-25799"},
		"taken on the target":      {rng: "25600-25699", used: []int{25610}},
		"target reports no range":  {rng: ""},
	} {
		t.Run(name, func(t *testing.T) {
			o, fs, sim, _ := newDirectFlow(t, tc.rng)
			fs.srv.HostPort = 25610
			fs.usedPorts = map[int][]int{2: tc.used}
			o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
			time.Sleep(100 * time.Millisecond)
			if len(fs.cutovers) != 1 {
				t.Fatalf("server not moved: cutovers %v", fs.cutovers)
			}
			if p, ok := sentHostPort(t, sim); ok {
				t.Fatalf("hostPort %v sent, want none so the target allocates", p)
			}
		})
	}
}

// With the gateway the route still follows the server, and no host port is
// pinned: gateway servers bind none.
func TestAGatewayMoveStillRepointsTheRoute(t *testing.T) {
	o, fs, sim, _ := newFlow(t, "stopped")
	routeMoves := 0
	o.gateway = flowGateway{routeMoves: &routeMoves}
	fs.srv.HostPort = 25610
	o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
	time.Sleep(100 * time.Millisecond)
	if routeMoves != 1 {
		t.Fatalf("route re-pointed %d times, want once", routeMoves)
	}
	if p, ok := sentHostPort(t, sim); ok {
		t.Fatalf("hostPort %v sent on a gateway move", p)
	}
}

// A proxy and its backends find each other on one node's network; a direct
// move of either half would cut them apart. Refused before anything stops.
func TestADirectModeMoveOfAProxyNetworkIsRefused(t *testing.T) {
	proxyID := 9
	for name, set := range map[string]func(*flowStore){
		"a proxy":             func(fs *flowStore) { fs.srv.ServerType = "proxy" },
		"a server behind one": func(fs *flowStore) { fs.srv.ProxyID = &proxyID },
	} {
		t.Run(name, func(t *testing.T) {
			o, fs, sim, _ := newDirectFlow(t, "25600-25699")
			fs.srv.Status = "online"
			set(fs)
			o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
			time.Sleep(400 * time.Millisecond)
			if len(fs.cutovers) != 0 || len(sim.commands(flowSourceToken, "stop")) != 0 || len(sim.commands(flowSourceToken, "migrate_out")) != 0 {
				t.Fatalf("a proxy-bound server was touched: cutovers %v, status %q", fs.cutovers, fs.status())
			}
		})
	}
}

func TestDirectMoveProblem(t *testing.T) {
	proxyID := 3
	cases := []struct {
		name string
		mode string
		srv  func() *flowStore
		want bool
	}{
		{"plain server, direct", "ip_port", func() *flowStore { return &flowStore{} }, false},
		{"proxy, direct", "ip_port", func() *flowStore { f := &flowStore{}; f.srv.ServerType = "proxy"; return f }, true},
		{"backend, direct", "ip_port", func() *flowStore { f := &flowStore{}; f.srv.ProxyID = &proxyID; return f }, true},
		{"backend, gateway", "gateway", func() *flowStore { f := &flowStore{}; f.srv.ProxyID = &proxyID; return f }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := c.srv()
			fs.mode = c.mode
			if got := DirectMoveProblem(fs, &fs.srv) != ""; got != c.want {
				t.Fatalf("refused=%v, want %v", got, c.want)
			}
		})
	}
}

// The target drops the archived config, so the container port an admin set
// came back as the node default and the server listened where nothing bound.
func TestAMoveCarriesTheContainerPort(t *testing.T) {
	for name, mode := range map[string]string{"direct": "ip_port", "gateway": ""} {
		t.Run(name, func(t *testing.T) {
			o, fs, sim, _ := newDirectFlow(t, "25600-25699")
			fs.mode = mode
			fs.srv.ContainerPort = 25577
			o.Migrate(context.Background(), MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"})
			time.Sleep(100 * time.Millisecond)
			got := sim.commands(flowTargetToken, "reconfigure")
			if len(got) != 1 {
				t.Fatalf("reconfigure sent %d times, want once", len(got))
			}
			d := got[0]["config"].(map[string]interface{})["docker"].(map[string]interface{})
			if d["containerPort"] != float64(25577) {
				t.Fatalf("containerPort sent %v, want 25577", d["containerPort"])
			}
		})
	}
}
