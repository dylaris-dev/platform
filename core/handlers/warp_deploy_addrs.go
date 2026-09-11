package handlers

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
)

// Two addresses exist only inside the overlay: Core's gRPC and Redis. Core is
// the only party that can name them - it is talking to Redis right now and it
// is itself on the same network - and it hands them to the machine's warp,
// which proxies both on fixed local ports. Nothing else on that machine ever
// learns an overlay address, so an overlay that moves is a value Core changes
// rather than a compose file every customer has to edit.
//
// Both are resolved to literal IPs on purpose. Core reaches Redis through a
// Swarm service name; a warp spoke has no DNS into the cluster, so handing it
// "redis:6379" produces a proxy that never resolves anything.

// DeployConfig is what a BYON or route-only deploy snippet still needs from
// Core: the tunnel subnets and the pin for Core's control channel. The two
// overlay addresses used to be here too, and now go to the machine's warp
// instead, which proxies them. An empty TunnelSubnets means "could not be
// determined here" and the panel keeps showing its placeholder rather than a
// plausible wrong value.
type DeployConfig struct {
	TunnelSubnets string `json:"tunnelSubnets"`
	// GRPCTLSFingerprint is Core's gRPC certificate fingerprint while
	// GRPC_TLS_ENABLED is on and "" while the channel is plaintext - the same
	// value, under the same rule, as the enroll-token answer. Every kit shown
	// WITHOUT a fresh enroll token (Update this machine, Deploy file, a rolled
	// key, the operator's deploy dialog) had nowhere else to read it, so it wrote
	// GRPC_TLS_ENABLED "false" against a TLS Core and a node deployed from it
	// lost its control channel. "" cannot mean "TLS on, pin unknown": the gRPC
	// server derives the very certificate this hashes, or does not start.
	GRPCTLSFingerprint string `json:"grpcTlsFingerprint"`
	// There is deliberately no cnameTarget here any more. It carried
	// gateway_cname_target verbatim, which is a LABEL ("route") and not a name,
	// and its single reader printed it to the customer as the record to create.
	// The custom-domain panel reads /api/gateway/route-options instead, which
	// carries the hoster bases the label has to be combined with - the same
	// source the route picker and the gateway settings tab already used.
}

// defaultCoreServiceName is the name Core answers to inside the overlay. It is
// one of the four load-bearing service names in the production stack, so the
// default is right for every deployment that follows it; CORE_SERVICE_NAME
// covers the ones that do not.
const defaultCoreServiceName = "core"

// overlayRedisAddr resolves Core's own REDIS_ADDR to host:port with a literal
// private IPv4 host. Returns "" when it cannot - which is the honest answer
// from a Core that reaches Redis some other way, and leaves the snippet's
// placeholder in place.
func overlayRedisAddr(redisEnv string) string {
	return resolveOverlayRedisAddr(redisEnv, net.LookupIP)
}

func resolveOverlayRedisAddr(redisEnv string, lookup func(string) ([]net.IP, error)) string {
	return resolveOverlayAddr(redisEnv, "6379", lookup)
}

// overlayCoreAddr resolves Core's OWN service name, so the snippet names the
// service rather than the instance that happened to answer the request.
//
// Core's task IP - what this used to hand out - changes on every redeploy and
// names exactly one replica when Core is scaled, while the RedisAddr beside it
// has always been a resolved service name. The two were inconsistent and only
// one of them was stable.
func overlayCoreAddr(grpcPort string) string {
	name := strings.TrimSpace(os.Getenv("CORE_SERVICE_NAME"))
	if name == "" {
		name = defaultCoreServiceName
	}
	return resolveOverlayAddr(name, grpcPort, net.LookupIP)
}

// resolveOverlayAddr turns a service name or literal address into "ip:port" with
// a literal private IPv4 host, using defaultPort when the input carries none.
//
// The resolver is injected so the behaviour can be pinned without depending on
// what DNS a machine happens to have. That is not hypothetical: CI runs behind a
// resolver that answers NXDOMAIN with a public address of its own.
//
// Which is also why the result must be PRIVATE. Core reaches both Redis and its
// own service over an overlay, which is RFC1918 by construction, so a public
// answer did not name the overlay - it is a hijacked lookup or a
// misconfiguration. Handing it out would point every customer's proxy at a
// stranger, which is worse than handing out nothing. Loopback is rejected for
// the same reason: it is never reachable from the machine that will dial it.
func resolveOverlayAddr(hostPort, defaultPort string, lookup func(string) ([]net.IP, error)) string {
	hostPort = strings.TrimSpace(hostPort)
	if hostPort == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		host, port = hostPort, defaultPort
	}

	usable := func(ip net.IP) string {
		v4 := ip.To4()
		if v4 == nil || !v4.IsPrivate() {
			return ""
		}
		return net.JoinHostPort(v4.String(), port)
	}

	if ip := net.ParseIP(host); ip != nil {
		return usable(ip)
	}
	ips, err := lookup(host)
	if err != nil {
		return ""
	}
	for _, ip := range ips {
		if addr := usable(ip); addr != "" {
			return addr
		}
	}
	return ""
}

// grpcPortFromEnv is the port half of every address Core hands out for its own
// gRPC endpoint.
func grpcPortFromEnv() string {
	if p := strings.TrimSpace(os.Getenv("DYLARIS_GRPC_PORT")); p != "" {
		return p
	}
	return "25501"
}

// overlayServiceAddrs resolves the two addresses a spoke's warp forwards to.
//
// This is the ONLY place that decides what "where is the overlay" means. If a
// service VIP ever turns out not to be reachable from a spoke, the answer
// changes here - to task IPs, or to whatever dnsrr resolves - and no machine on
// anyone's hardware is reconfigured: they all pick it up on their next poll.
func overlayServiceAddrs(grpcPort string) (coreAddr, redisAddr string) {
	redisEnv := os.Getenv("REDIS_ADDR")
	redisAddr = overlayRedisAddr(redisEnv)
	coreAddr = overlayCoreAddr(grpcPort)
	if coreAddr == "" {
		// Fallback for a Core that does not answer to its own service name -
		// a single-container install, or a stack that renamed the service
		// without setting CORE_SERVICE_NAME. The local address the kernel picks
		// to reach Redis, NOT the first private IP: Core sits in several
		// networks at once (the default bridge, the ingress overlay, a
		// reverse-proxy overlay, the service overlay) and only the Redis-facing
		// one is provably the overlay the spoke will be on. It is an instance
		// address, so it is second choice, not first.
		if local := localAddrToward(redisEnv); local != nil {
			coreAddr = net.JoinHostPort(local.String(), grpcPort)
		}
	}
	return coreAddr, redisAddr
}

// deployConfig answers with the tunnel subnets and the gRPC pin for a deploy
// snippet.
func (s *AppState) deployConfig() DeployConfig {
	var out DeployConfig
	if s != nil && s.GRPCTLSEnabled {
		out.GRPCTLSFingerprint = s.GRPCTLSFingerprint
	}
	if s != nil && s.Store != nil {
		v, _ := s.Store.GetSetting("warp_tunnel_subnets")
		out.TunnelSubnets = strings.TrimSpace(v)
	}
	// Nothing stored: fall back to the value Core can detect about its own
	// networks. The snippet is only useful if every field in it is filled, and
	// an operator who never opened the warp settings has no stored value - which
	// is the normal case, not an edge one.
	if out.TunnelSubnets == "" && s != nil {
		out.TunnelSubnets = s.suggestTunnelSubnets().Suggested
	}
	return out
}

// GetDeployConfig GET /api/warp/deploy-config - any authenticated user.
//
// Deliberately not admin-only: the tenant who mints a BYON key on /nodes is the
// one who needs this, and withholding it would only mean handing it over by
// email instead. It is an RFC1918 CIDR for an overlay nobody reaches without
// their own authenticated warp key, and the hash of a certificate every client
// of the gRPC port is shown in its handshake, so it authorizes nothing on its
// own.
func (h *WarpHandler) GetDeployConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"config":  h.state.deployConfig(),
	})
}
