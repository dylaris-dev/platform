package main

// On a BYON machine warp is the only thing that knows an overlay address. It
// listens on two fixed local ports and forwards over the tunnel to whatever
// Core says Redis and Core's gRPC are today, so when the platform overlay is
// rebuilt and both addresses move, nothing here has to be reconfigured - the
// connection drops once and reconnects.
//
// Reaching those ports is not the same question for everyone on the machine:
// the node is host-networked and uses loopback, while the link sidecar and every
// MC container sit on a Docker bridge where 127.0.0.1 is their own. They use the
// bridge gateway, which is the host, which is where warp listens.

import "strings"

const (
	// Ports warp binds locally. Compiled, not configured: warp compiles the
	// same two numbers in the gateway repo (gateway/warp/proxy.go) and there is
	// no channel between the two to negotiate a different pair. A machine with
	// a port collision sets CORE_GRPC_ADDR / REDIS_ADDR explicitly, which
	// bypasses the proxy entirely.
	warpProxyCorePort  = "25570"
	warpProxyRedisPort = "25571"
)

// warpProxyLoopback is the proxy as the NODE sees it.
func warpProxyLoopback(port string) string { return "127.0.0.1:" + port }

// resolveNodeAddr picks the address the node process itself dials.
//
// An explicit value always wins - an operator who set one meant it, and it is
// the escape hatch for a port collision or a topology we did not anticipate.
// Empty falls back to the local warp proxy, but only on an external node: there
// is no warp inside the cluster, and defaulting to loopback there would turn a
// visible configuration error into a silent one.
func resolveNodeAddr(env string, external bool, proxyPort string) (addr string, viaProxy bool) {
	if v := strings.TrimSpace(env); v != "" {
		return v, false
	}
	if !external {
		return "", false
	}
	return warpProxyLoopback(proxyPort), true
}

// resolveSidecarRedisAddr picks the Redis address baked into MC containers and
// the node-managed link sidecar.
//
// They are NOT in the node's network namespace, so the node's own answer is
// wrong for them whenever it is loopback. gateway is the bridge gateway of the
// network the container joins; empty means it could not be determined yet, and
// the caller must fail rather than bake in an address that cannot work.
func resolveSidecarRedisAddr(nodeAddr string, nodeViaProxy bool, gateway string) string {
	if !nodeViaProxy {
		// Routable from a container whenever it is not the proxy's loopback:
		// the node and its containers sit on the overlay Core's address names.
		return nodeAddr
	}
	if gateway == "" {
		return ""
	}
	return gateway + ":" + warpProxyRedisPort
}
