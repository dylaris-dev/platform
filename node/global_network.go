package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
)

// warpInterfacePrefix is the interface-name prefix the warp client brings its
// tunnel up under (gateway/warp: OwnedInterfacePrefix, default "dylaris-wg0").
//
// CROSS-REPO CONTRACT, and the reason it is a PREFIX rather than a fixed name:
// warp deliberately does not use "wg0", because a BYON machine very often
// already runs WireGuard under that name. This side only has to recognise the
// tunnel, so matching the prefix keeps the two repos from having to agree on an
// exact string - but the prefix itself must stay in step with warp's.
const warpInterfacePrefix = "dylaris-"

// warpRoutedSubnets parses /proc/net/route content and returns every subnet
// routed through the warp tunnel, so a freshly-created local dylaris_net can
// avoid shadowing it. Best-effort: an unparseable line is skipped and a fully
// unparseable table yields no ranges (Docker's own non-overlap and the 10/8
// reservation still apply).
//
// Matching the customer's OWN WireGuard interfaces here would be wrong: those
// subnets are theirs to keep, and reserving them would shrink the pool this node
// can pick from for no benefit.
func warpRoutedSubnets(routeTable string) []*net.IPNet {
	var out []*net.IPNet
	for _, line := range strings.Split(routeTable, "\n") {
		f := strings.Fields(line)
		// /proc/net/route columns: Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		if len(f) < 8 || !strings.HasPrefix(f[0], warpInterfacePrefix) {
			continue
		}
		dest, derr := hexLEtoIP(f[1])
		mask, merr := hexLEtoIP(f[7])
		if derr != nil || merr != nil {
			continue
		}
		// Skip a default route (0.0.0.0/0 via wg0, i.e. full-tunnel): it is not a
		// specific subnet to avoid. Treating it as reserved would make every
		// candidate overlap and collapse the whole avoidance to Docker auto-assign.
		if ones, _ := net.IPMask(mask).Size(); ones == 0 {
			continue
		}
		out = append(out, &net.IPNet{IP: dest, Mask: net.IPMask(mask)})
	}
	return out
}

// hexLEtoIP decodes a little-endian hex IPv4 as /proc/net/route stores it
// (e.g. "0000000A" -> 10.0.0.0).
func hexLEtoIP(h string) (net.IP, error) {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 4 {
		return nil, fmt.Errorf("bad hex ip %q", h)
	}
	return net.IPv4(b[3], b[2], b[1], b[0]).To4(), nil
}

// reservedSubnets unions the ranges a new local dylaris_net must avoid: existing
// Docker network subnets, every range routed through the warp tunnel, and - on
// a host-net node - all of 10.0.0.0/8, which is warp's home range and must never
// be shadowed by a local bridge. The result is fed to nextFreeSubnet, which then
// picks a free block from 172.16/12 or 192.168/16 on a host-net node.
func reservedSubnets(dockerSubnets []*net.IPNet, routeTable string, hostNet bool) []*net.IPNet {
	used := append([]*net.IPNet{}, dockerSubnets...)
	used = append(used, warpRoutedSubnets(routeTable)...)
	if hostNet {
		used = append(used, parseCIDRs([]string{"10.0.0.0/8"})...)
	}
	return used
}

// privatePools are the private ranges nextFreeSubnet draws blocks from, in
// order: 10/8 first (largest), then 172.16/12 and 192.168/16.
func privatePools() []*net.IPNet {
	out := make([]*net.IPNet, 0, 3)
	for _, c := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		_, n, _ := net.ParseCIDR(c) // literals: err is always nil
		out = append(out, n)
	}
	return out
}

// cidrsOverlap reports whether two IPv4 CIDR ranges share any address.
func cidrsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

// parseCIDRs parses CIDR strings, skipping empties and non-IPv4/unparseable
// entries (a Docker network without an IPAM subnet yields "").
func parseCIDRs(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil || n.IP.To4() == nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// nextFreeSubnet scans privatePools in order and returns the first subnet of the
// given prefix length not overlapping any subnet in used. Errors when exhausted.
func nextFreeSubnet(used []*net.IPNet, prefixLen int) (*net.IPNet, error) {
	step := uint32(1) << (32 - prefixLen)
	mask := net.CIDRMask(prefixLen, 32)
	for _, pool := range privatePools() {
		poolStart := binary.BigEndian.Uint32(pool.IP.To4())
		poolOnes, _ := pool.Mask.Size()
		poolEnd := poolStart + (uint32(1) << (32 - poolOnes)) // exclusive
		for base := poolStart; base >= poolStart && base+step <= poolEnd; base += step {
			ipBytes := make(net.IP, 4)
			binary.BigEndian.PutUint32(ipBytes, base)
			cand := &net.IPNet{IP: ipBytes, Mask: mask}
			overlap := false
			for _, u := range used {
				if cidrsOverlap(cand, u) {
					overlap = true
					break
				}
			}
			if !overlap {
				return cand, nil
			}
		}
	}
	return nil, fmt.Errorf("no free /%d block in the private pools", prefixLen)
}
