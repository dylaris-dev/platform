package main

import (
	"net"
	"strings"
)

// redisAddrIsolationSafe reports whether SIDECAR_REDIS_ADDR (the address baked
// into isolated MC containers) is reachable WITHOUT Docker DNS on the shared
// dylaris_net. Isolated servers leave dylaris_net, so a Docker-DNS-only address
// (a bare single-label name like "redis") would leave the log-shipper unable to
// reach Redis. Fails safe: anything not clearly host-reachable returns false, so
// the node keeps that node's servers on dylaris_net instead of isolating them.
// missingSidecarRedisAddr reports whether the node was left without the one
// address it must be told rather than derive.
//
// The exception is the whole reason this is a function and not an inline `==
// ""`. On the local warp proxy an empty value is not an omission: a container
// reaches warp through the gateway of its OWN network, so there is no single
// address to configure and the node resolves one per network when it creates
// the container. Requiring the variable there would refuse to start every proxy
// node for failing to set something that cannot have a value.
func missingSidecarRedisAddr(sidecarEnv string, viaWarpProxy bool) bool {
	if strings.TrimSpace(sidecarEnv) != "" {
		return false
	}
	return !viaWarpProxy
}

func redisAddrIsolationSafe(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return true // explicit IP: routable off the Docker network
	}
	if host == "host.docker.internal" {
		return true // Docker's host-gateway alias
	}
	// A dotted name is a real (resolvable) FQDN. A bare single-label name
	// (e.g. "redis", "valkey") only resolves via Docker DNS on a shared net.
	return strings.Contains(host, ".")
}
