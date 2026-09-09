package main

import "testing"

func TestRedisAddrIsolationSafe(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want bool
	}{
		{"empty is unsafe", "", false},
		{"bare single-label name is dns-only", "redis", false},
		{"bare name with port is dns-only", "redis:6379", false},
		{"other bare service name is dns-only", "valkey:6379", false},
		{"private ipv4 with port is safe", "10.0.0.5:6379", true},
		{"loopback ipv4 is safe", "127.0.0.1:6379", true},
		{"public ipv4 is safe", "94.130.98.3:6379", true},
		{"docker host gateway alias is safe", "host.docker.internal:6379", true},
		{"dotted fqdn is safe", "redis.internal.example.com:6379", true},
		{"bracketed ipv6 with port is safe", "[::1]:6379", true},
		{"whitespace is trimmed", "  10.0.0.5:6379  ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := redisAddrIsolationSafe(c.addr); got != c.want {
				t.Fatalf("redisAddrIsolationSafe(%q) = %v, want %v", c.addr, got, c.want)
			}
		})
	}
}

// SIDECAR_REDIS_ADDR is now required, and the exception is the part that can be
// "simplified" away by somebody who reads the rule as `env == ""`.
//
// A node on the local warp proxy legitimately has no value to give: a container
// reaches warp through the gateway of its own network, so the address is
// resolved per network when the container is created. Requiring it there would
// refuse to start every proxy node over a setting that cannot have one value.
func TestMissingSidecarRedisAddr(t *testing.T) {
	tests := []struct {
		name         string
		env          string
		viaWarpProxy bool
		want         bool
		why          string
	}{
		{
			name: "unset on an ordinary node", want: true,
			why: "it used to fall back to REDIS_ADDR and quietly decide isolation with it",
		},
		{
			// Both shipped compose files write "" when the operator sets
			// nothing, and a set-but-empty variable overrides a code default -
			// so this IS the common case, not an edge one.
			name: "whitespace only", env: "   ", want: true,
			why: "an empty string is not an address",
		},
		{
			name: "set", env: "10.0.0.5:6379", want: false,
		},
		{
			name: "unset behind the local warp proxy", viaWarpProxy: true, want: false,
			why: "empty is the instruction to resolve per network, not an omission",
		},
		{
			name: "set behind the local warp proxy", env: "10.0.0.5:6379", viaWarpProxy: true, want: false,
			why: "an explicit value still wins there",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := missingSidecarRedisAddr(tt.env, tt.viaWarpProxy); got != tt.want {
				t.Errorf("missingSidecarRedisAddr(%q, %v) = %v, want %v (%s)",
					tt.env, tt.viaWarpProxy, got, tt.want, tt.why)
			}
		})
	}
}

// The property that keeps this requirement off customers' machines.
//
// A BYON node is external and is handed no REDIS_ADDR - the deploy ENV the
// panel generates sets neither variable - so it resolves Redis through warp's
// local proxy, and the per-network exception applies. Requiring the variable
// there would refuse to start every customer's node on their next update, over
// a setting the panel never told them about.
//
// The two functions are composed here on purpose: each is fine on its own and
// the guarantee lives in the join between them.
func TestAnExternalNodeIsNotRequiredToSetTheSidecarAddress(t *testing.T) {
	cases := []struct {
		name      string
		redisAddr string
		external  bool
		want      bool
		why       string
	}{
		{
			name: "BYON: external, no REDIS_ADDR", external: true, want: false,
			why: "reaches Redis through the local warp proxy, where the address is per network",
		},
		{
			name: "cluster node: REDIS_ADDR set, no sidecar address", redisAddr: "10.0.0.5:6379", want: true,
			why: "this is the case the requirement exists for",
		},
		{
			name: "external node that was given an explicit REDIS_ADDR", redisAddr: "10.0.0.5:6379", external: true, want: true,
			why: "an explicit address means it is not on the proxy path, so the rule applies again",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, viaProxy := resolveNodeAddr(tt.redisAddr, tt.external, warpProxyRedisPort)
			if got := missingSidecarRedisAddr("", viaProxy); got != tt.want {
				t.Errorf("required = %v, want %v (%s)", got, tt.want, tt.why)
			}
		})
	}
}
