package main

import "testing"

func TestResolveNodeAddr(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		external  bool
		wantAddr  string
		wantProxy bool
	}{
		{
			name:     "explicit value wins on an external node",
			env:      "10.20.0.5:6379",
			external: true,
			wantAddr: "10.20.0.5:6379",
		},
		{
			// The escape hatch for a port collision or a topology we did not
			// anticipate, so it must beat the proxy rather than the other way.
			name:     "explicit value wins in the cluster too",
			env:      "redis:6379",
			external: false,
			wantAddr: "redis:6379",
		},
		{
			name:      "unset on an external node falls back to the local proxy",
			env:       "",
			external:  true,
			wantAddr:  "127.0.0.1:25571",
			wantProxy: true,
		},
		{
			name:      "whitespace counts as unset",
			env:       "   ",
			external:  true,
			wantAddr:  "127.0.0.1:25571",
			wantProxy: true,
		},
		{
			// There is no warp inside the cluster. Defaulting to loopback here
			// would turn a visible configuration error into a silent one, so
			// the empty answer is kept and the caller still fails loudly.
			name:     "unset in the cluster stays empty",
			env:      "",
			external: false,
			wantAddr: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, viaProxy := resolveNodeAddr(tt.env, tt.external, warpProxyRedisPort)
			if addr != tt.wantAddr {
				t.Errorf("addr = %q, want %q", addr, tt.wantAddr)
			}
			if viaProxy != tt.wantProxy {
				t.Errorf("viaProxy = %v, want %v", viaProxy, tt.wantProxy)
			}
		})
	}
}

func TestResolveNodeAddrUsesTheGivenPort(t *testing.T) {
	addr, _ := resolveNodeAddr("", true, warpProxyCorePort)
	if addr != "127.0.0.1:25570" {
		t.Errorf("core addr = %q, want 127.0.0.1:25570", addr)
	}
}

// The node's own answer is wrong for anything in a container whenever it is
// loopback: an MC container and the link sidecar sit on a bridge, where
// 127.0.0.1 is their own. This is the case that made the first design fail.
func TestResolveSidecarRedisAddr(t *testing.T) {
	tests := []struct {
		name     string
		nodeAddr string
		viaProxy bool
		gateway  string
		want     string
	}{
		{
			name:     "no proxy: the node's own address",
			nodeAddr: "10.20.0.5:6379",
			viaProxy: false,
			want:     "10.20.0.5:6379",
		},
		{
			name:     "proxy: the bridge gateway, never the node's loopback",
			nodeAddr: "127.0.0.1:25571",
			viaProxy: true,
			gateway:  "172.18.0.1",
			want:     "172.18.0.1:25571",
		},
		{
			// Better no container than a container that comes up healthy and
			// ships no console output at all.
			name:     "proxy with an unknown gateway yields nothing",
			nodeAddr: "127.0.0.1:25571",
			viaProxy: true,
			gateway:  "",
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveSidecarRedisAddr(tt.nodeAddr, tt.viaProxy, tt.gateway)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
