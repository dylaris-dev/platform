package main

import (
	"net"
	"strings"
	"testing"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("bad CIDR %q: %v", s, err)
	}
	return n
}

func TestCidrsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"10.0.0.0/26", "10.0.0.0/26", true},
		{"10.0.0.0/26", "10.0.0.32/27", true}, // b inside a
		{"10.0.0.0/25", "10.0.0.0/26", true},  // b inside a (wider a)
		{"10.0.0.0/26", "10.0.0.64/26", false},
		{"10.0.0.0/26", "172.16.0.0/26", false},
	}
	for _, c := range cases {
		if got := cidrsOverlap(mustCIDR(t, c.a), mustCIDR(t, c.b)); got != c.want {
			t.Errorf("cidrsOverlap(%s,%s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestParseCIDRsSkipsJunk(t *testing.T) {
	got := parseCIDRs([]string{"10.0.0.0/26", "", "  ", "not-a-cidr", "fd00::/64", "192.168.5.0/24"})
	if len(got) != 2 {
		t.Fatalf("parseCIDRs kept %d nets, want 2 (%v)", len(got), got)
	}
	if got[0].String() != "10.0.0.0/26" || got[1].String() != "192.168.5.0/24" {
		t.Fatalf("parseCIDRs = %v, want [10.0.0.0/26 192.168.5.0/24]", got)
	}
}

func TestNextFreeSubnetAvoidsOverlaps(t *testing.T) {
	t.Run("empty used -> first block", func(t *testing.T) {
		got, err := nextFreeSubnet(nil, 26)
		if err != nil {
			t.Fatalf("nextFreeSubnet: %v", err)
		}
		if got.String() != "10.0.0.0/26" {
			t.Fatalf("got %s, want 10.0.0.0/26", got)
		}
	})
	t.Run("skips used blocks in order", func(t *testing.T) {
		used := []*net.IPNet{mustCIDR(t, "10.0.0.0/26"), mustCIDR(t, "10.0.0.64/26")}
		got, err := nextFreeSubnet(used, 26)
		if err != nil {
			t.Fatalf("nextFreeSubnet: %v", err)
		}
		if got.String() != "10.0.0.128/26" {
			t.Fatalf("got %s, want 10.0.0.128/26", got)
		}
	})
	t.Run("skips a wide used block", func(t *testing.T) {
		// A used /24 covering 10.0.0.0-10.0.0.255 pushes the next /26 to 10.0.1.0.
		used := []*net.IPNet{mustCIDR(t, "10.0.0.0/24")}
		got, err := nextFreeSubnet(used, 26)
		if err != nil {
			t.Fatalf("nextFreeSubnet: %v", err)
		}
		if got.String() != "10.0.1.0/26" {
			t.Fatalf("got %s, want 10.0.1.0/26", got)
		}
	})
}

func TestWarpRoutedSubnets(t *testing.T) {
	// /proc/net/route stores Destination + Mask as little-endian hex.
	// wg0 -> 10.0.0.0/16: Destination=0000000A, Mask=0000FFFF
	// wg0 -> 10.77.0.0/16: Destination=00004D0A, Mask=0000FFFF
	table := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"dylaris-wg0\t0000000A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
		"dylaris-wg0\t00004D0A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
		"eth0\t00000000\t0100000A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
	nets := warpRoutedSubnets(table)
	if len(nets) != 2 {
		t.Fatalf("warpRoutedSubnets len = %d, want 2 (%v)", len(nets), nets)
	}
	if nets[0].String() != "10.0.0.0/16" || nets[1].String() != "10.77.0.0/16" {
		t.Fatalf("warpRoutedSubnets = %v, want [10.0.0.0/16 10.77.0.0/16]", nets)
	}
}

func TestWg0RoutedSubnetsIgnoresGarbage(t *testing.T) {
	if got := warpRoutedSubnets(""); len(got) != 0 {
		t.Fatalf("empty table -> %v, want none", got)
	}
	if got := warpRoutedSubnets("dylaris-wg0 short line\nnonsense"); len(got) != 0 {
		t.Fatalf("garbage -> %v, want none", got)
	}
}

// A full-tunnel default route (0.0.0.0/0 via wg0) must be skipped: treating it as
// a reserved subnet would make every candidate overlap and collapse avoidance.
func TestWg0RoutedSubnetsSkipsDefaultRoute(t *testing.T) {
	table := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"dylaris-wg0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"dylaris-wg0\t0000000A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	nets := warpRoutedSubnets(table)
	if len(nets) != 1 || nets[0].String() != "10.0.0.0/16" {
		t.Fatalf("warpRoutedSubnets = %v, want just [10.0.0.0/16] (default route skipped)", nets)
	}
}

// On a host-net node the chosen local subnet must avoid every Docker subnet, the
// wg0 tunnel ranges, AND all of 10/8 (warp's home) - so nextFreeSubnet lands in
// 172.16/12.
func TestReservedSubnetsHostNetPushesOutOf10Slash8(t *testing.T) {
	docker := []*net.IPNet{mustCIDR(t, "172.17.0.0/16")}
	table := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"dylaris-wg0\t0000000A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	used := reservedSubnets(docker, table, true)

	free, err := nextFreeSubnet(used, 24)
	if err != nil {
		t.Fatalf("nextFreeSubnet: %v", err)
	}
	// Must be a private /24 that overlaps nothing reserved, and NOT inside 10/8.
	if ten := mustCIDR(t, "10.0.0.0/8"); cidrsOverlap(free, ten) {
		t.Fatalf("host-net pick %s is inside 10/8 (warp home)", free)
	}
	for _, u := range used {
		if cidrsOverlap(free, u) {
			t.Fatalf("pick %s overlaps reserved %s", free, u)
		}
	}
}

// Off a host-net node, 10/8 is NOT reserved wholesale - only the specific wg0
// ranges and Docker subnets are avoided.
func TestReservedSubnetsNonHostNetKeeps10Slash8(t *testing.T) {
	used := reservedSubnets(nil, "", false)
	if len(used) != 0 {
		t.Fatalf("non-host-net with no docker nets / no routes -> %v, want none reserved", used)
	}
}

// The customer's OWN WireGuard interfaces must not be treated as warp's. warp
// deliberately avoids the name "wg0" precisely because BYON machines often
// already run WireGuard there; reserving those subnets would shrink this node's
// pool for no benefit, and matching on the old literal would make the whole
// avoidance silently return nothing once warp renamed its interface.
func TestWarpRoutedSubnetsIgnoresForeignInterfaces(t *testing.T) {
	table := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"wg0\t0000000A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
		"tailscale0\t00004D0A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
		"eth0\t0000A8C0\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	if got := warpRoutedSubnets(table); len(got) != 0 {
		t.Errorf("foreign interfaces were treated as the warp tunnel: %v", got)
	}
}

// The prefix this node matches on must stay in step with the interface name warp
// actually brings up (gateway/warp OwnedInterfacePrefix + DefaultWGInterface).
// Nothing links the two repos, so this pins the contract from this side.
func TestWarpInterfacePrefixMatchesWarpsDefault(t *testing.T) {
	const warpDefaultInterface = "dylaris-wg0" // gateway/warp: DefaultWGInterface
	if !strings.HasPrefix(warpDefaultInterface, warpInterfacePrefix) {
		t.Fatalf("warpInterfacePrefix %q does not match warp's default interface %q",
			warpInterfacePrefix, warpDefaultInterface)
	}
}
