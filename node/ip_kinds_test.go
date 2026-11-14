package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// ms01 as it reported itself: a LAN NIC, docker0, two compose bridges, warp's
// interface and a WireGuard interface of the owner's own. Only the NIC is the
// machine's address; a LAN that happens to be in 172.16/12 still is one.
func TestClassifyPrivateIPs(t *testing.T) {
	sys := t.TempDir()
	for name, uevent := range map[string]string{
		"enp3s0":     "INTERFACE=enp3s0\n",
		"docker0":    "DEVTYPE=bridge\nINTERFACE=docker0\n",
		"wg0":        "DEVTYPE=wireguard\nINTERFACE=wg0\n",
		"eth1":       "INTERFACE=eth1\n",
		"br-1a2b3c4": "DEVTYPE=bridge\n",
	} {
		os.MkdirAll(filepath.Join(sys, name), 0o755)
		os.WriteFile(filepath.Join(sys, name, "uevent"), []byte(uevent), 0o644)
	}
	cidr := func(s string) *net.IPNet { _, n, _ := net.ParseCIDR(s); return n }
	dockerNets := []*net.IPNet{cidr("172.17.0.0/16"), cidr("172.18.0.0/16")}
	got := classifyPrivateIPs(map[string][]net.IP{
		"enp3s0":      {net.ParseIP("10.10.10.8"), net.ParseIP("fe80::1")},
		"eth1":        {net.ParseIP("172.20.5.9")},
		"docker0":     {net.ParseIP("172.17.0.1")},
		"br-1a2b3c4":  {net.ParseIP("172.18.0.1")},
		"dylaris-wg0": {net.ParseIP("10.20.0.7")},
		"wg0":         {net.ParseIP("10.99.0.2")},
		"lo":          {net.ParseIP("127.0.0.1")},
		"tailscale0":  {net.ParseIP("100.64.0.3")},
	}, sys, dockerNets)
	want := map[string]string{
		"10.10.10.8": "lan",
		"172.20.5.9": "lan",
		"172.17.0.1": "docker",
		"172.18.0.1": "docker",
		"10.20.0.7":  "overlay",
		"10.99.0.2":  "overlay",
	}
	if len(got) != len(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for ip, k := range want {
		if got[ip] != k {
			t.Errorf("%s: %q, want %q", ip, got[ip], k)
		}
	}
}

func TestPickServerNetwork(t *testing.T) {
	for _, c := range []struct {
		name     string
		names    []string
		override string
		want     string
	}{
		{"override listed", []string{"bridge", "kit_net", "x_dylaris_net"}, "kit_net", "kit_net"},
		{"override missing", []string{"bridge", "x_dylaris_net"}, "kit_net", ""},
		{"one prefixed match", []string{"bridge", "host", "x_dylaris_net"}, "", "x_dylaris_net"},
		{"exact match", []string{"dylaris_net"}, "", "dylaris_net"},
		{"two matches", []string{"a_dylaris_net", "b_dylaris_net"}, "", ""},
		{"none", []string{"bridge", "host", "none"}, "", ""},
		{"control character", []string{"x\n_dylaris_net"}, "", ""},
	} {
		if got := pickServerNetwork(c.names, c.override); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBeamTargetFor(t *testing.T) {
	_, overlay, _ := net.ParseCIDR("10.0.5.0/24")
	_, bridge, _ := net.ParseCIDR("172.21.0.0/16")
	for _, c := range []struct {
		name    string
		subnets []*net.IPNet
		addrs   []net.IP
		want    string
	}{
		{"container address in subnet", []*net.IPNet{overlay},
			[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("172.18.0.4"), net.ParseIP("10.0.5.17")}, "10.0.5.17:25521"},
		{"host bridge gateway in subnet", []*net.IPNet{bridge},
			[]net.IP{net.ParseIP("192.168.1.20"), net.ParseIP("172.21.0.1")}, "172.21.0.1:25521"},
		{"outside every subnet", []*net.IPNet{overlay, bridge},
			[]net.IP{net.ParseIP("192.168.1.20"), net.ParseIP("10.0.6.1")}, ""},
		{"IPv6 only", []*net.IPNet{overlay},
			[]net.IP{net.ParseIP("fd00::5")}, ""},
		{"no subnets", nil, []net.IP{net.ParseIP("10.0.5.17")}, ""},
	} {
		if got := beamTargetFor(c.subnets, c.addrs, "25521"); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The two keys are additive: an unknown fact adds no key at all (absent is
// "unknown" to Core, "" would not be), a known one adds exactly its key, and
// nothing the heartbeat already carried changes.
func TestHeartbeatNetworkKeysAreAdditive(t *testing.T) {
	// Every key sendHeartbeat can write before this change.
	golden := []string{"id", "name", "ip", "tags", "region", "timestamp", "ips", "hostname", "sig",
		"cpuUsage", "ramFree", "ramTotal", "totalCpu", "linkCount", "storage", "sharedStorage",
		"portRange", "portRangeNotice", "netPolicy", "netPolicyServers", "netPolicyNotice",
		"noMemoryLimit", "releaseVersion", "linkKeySwap"}
	base := func() map[string]interface{} {
		m := map[string]interface{}{}
		for i, k := range golden {
			m[k] = i
		}
		return m
	}
	for _, c := range []struct {
		name, net, target string
		added             map[string]string
	}{
		{"unknown", "", "", map[string]string{}},
		{"known", "x_dylaris_net", "10.0.5.17:25521", map[string]string{"serverNetwork": "x_dylaris_net", "beamTarget": "10.0.5.17:25521"}},
		{"network without target", "x_dylaris_net", "", map[string]string{"serverNetwork": "x_dylaris_net"}},
	} {
		data := base()
		addServerNetworkFacts(data, c.net, c.target)
		if len(data) != len(golden)+len(c.added) {
			t.Errorf("%s: %d keys, want %d: %v", c.name, len(data), len(golden)+len(c.added), data)
		}
		for i, k := range golden {
			if data[k] != i {
				t.Errorf("%s: existing key %q changed to %v", c.name, k, data[k])
			}
		}
		for k, v := range c.added {
			if data[k] != v {
				t.Errorf("%s: %q = %v, want %q", c.name, k, data[k], v)
			}
		}
	}
}
