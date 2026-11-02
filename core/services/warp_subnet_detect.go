package services

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// detectOverlaySubnets works out which CIDR(s) a warp client has to route
// through the tunnel, so a self-hoster never has to look the value up.
//
// The requirement is precise: a client must reach the network where Redis and
// Core gRPC live. Core is itself in that network, so instead of guessing from
// interface names (a container sits in several networks - the Docker default
// bridge, the ingress overlay, a reverse-proxy overlay, the service overlay -
// and none of them is identifiable by name), we ask the routing table which
// local address is used to reach Redis, then read the PREFIX from the interface
// that owns it. Docker hands a container the overlay's own mask, so an address
// of 10.20.0.5/16 yields exactly 10.20.0.0/16.
//
// Ordering: the Redis-derived prefix comes first because it is the one that is
// provably right. Remaining private prefixes follow as fallbacks for the case
// where the probe cannot resolve (Redis behind a hostname that only resolves
// inside another netns, no Redis configured yet).
//
// Returns network addresses, deduped, most-confident first. An empty result
// means "cannot tell" and the caller keeps showing a placeholder rather than
// inventing a range.
func detectOverlaySubnets(redisAddr string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(cidr string) {
		if cidr != "" && !seen[cidr] {
			seen[cidr] = true
			out = append(out, cidr)
		}
	}

	if local := LocalAddrToward(redisAddr); local != nil {
		add(prefixForLocalIP(local))
	}
	for _, cidr := range privateInterfacePrefixes() {
		add(cidr)
	}
	return out
}

// LocalAddrToward returns the local IP the kernel would use to reach addr. A UDP
// "connect" only selects a route and sends nothing, so this is free and works
// even when Redis is down - it is the routing decision we want, not reachability.
func LocalAddrToward(addr string) net.IP {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "6379"
	}
	// A hostname is resolved first: "redis" only resolves inside the overlay,
	// which is precisely the case we want to succeed.
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return nil
	}
	var target net.IP
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			target = v4
			break
		}
	}
	if target == nil {
		return nil
	}
	conn, err := net.DialTimeout("udp", net.JoinHostPort(target.String(), port), 2*time.Second)
	if err != nil {
		return nil
	}
	defer conn.Close()
	ua, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil
	}
	return ua.IP.To4()
}

// prefixForLocalIP finds the interface holding ip and returns its network in
// CIDR form. "" when the address is not on any up interface, or is not private -
// a public address is never a tunnel subnet.
func prefixForLocalIP(ip net.IP) string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, aerr := iface.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			if !ipnet.IP.To4().Equal(ip) {
				continue
			}
			if !ipnet.IP.IsPrivate() {
				return ""
			}
			return (&net.IPNet{IP: ipnet.IP.Mask(ipnet.Mask), Mask: ipnet.Mask}).String()
		}
	}
	return ""
}

// privateInterfacePrefixes lists the private IPv4 networks this host is attached
// to, skipping loopback and the Docker default bridges (172.17/172.18): those
// are per-host plumbing, never a network a remote client should route into.
func privateInterfacePrefixes() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, aerr := iface.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			v4 := ipnet.IP.To4()
			if v4 == nil || !v4.IsPrivate() {
				continue
			}
			if v4[0] == 172 && (v4[1] == 17 || v4[1] == 18) {
				continue
			}
			out = append(out, (&net.IPNet{IP: v4.Mask(ipnet.Mask), Mask: ipnet.Mask}).String())
		}
	}
	return out
}

// SuggestedTunnelSubnets is what the panel shows when nothing is stored: the
// detected value, ready to save, plus the alternatives so an operator on a
// multi-network Core can pick the right one instead of trusting a single guess.
type SuggestedTunnelSubnets struct {
	// Suggested is the best candidate, pre-filled into the field. "" = unknown.
	Suggested string `json:"suggested"`
	// Candidates includes Suggested, most-confident first.
	Candidates []string `json:"candidates"`
	// Source explains WHY Suggested was chosen, so the value is auditable
	// rather than magic.
	Source string `json:"source"`
}

// SuggestTunnelSubnets is the tunnel-subnet suggestion the warp settings and
// the deploy snippet show when nothing is stored.
func SuggestTunnelSubnets() SuggestedTunnelSubnets {
	// Read from the environment rather than threading config through AppState:
	// this is the same value config.Load reads, and it is only ever used for a
	// suggestion, so a stale copy could not cause a wrong decision anywhere.
	redisAddr := strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	cands := detectOverlaySubnets(redisAddr)
	out := SuggestedTunnelSubnets{Candidates: cands}
	if len(cands) == 0 {
		out.Source = "could not be detected on this host"
		return out
	}
	out.Suggested = cands[0]
	out.Source = fmt.Sprintf("detected from the network Core reaches Redis (%s) over", redisAddr)
	if redisAddr == "" {
		out.Source = "detected from this host's private networks"
	}
	return out
}
