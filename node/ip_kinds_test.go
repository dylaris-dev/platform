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
