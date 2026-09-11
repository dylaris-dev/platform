package handlers

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/services"
)

// Every node kit shown without a fresh enroll token reads Core's gRPC pin from
// here. It follows GRPC_TLS_ENABLED exactly as the enroll-token answer does: the
// pin while TLS is on, "" while the channel is plaintext - which the panel writes
// as GRPC_TLS_ENABLED "false", so a pin shown while TLS is off would be as wrong
// as none while it is on. The field is always present: the panel reads a missing
// one as "not known", never as plaintext.
func TestGetDeployConfigCarriesTheGRPCPinOnlyWhileTLSIsOn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		want    string
	}{
		{"TLS on", true, "ab12cd34"},
		{"TLS off", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewWarpHandler(&AppState{GRPCTLSEnabled: tc.enabled, GRPCTLSFingerprint: "ab12cd34"}, nil)
			rec := httptest.NewRecorder()
			h.GetDeployConfig(rec, httptest.NewRequest(http.MethodGet, "/api/warp/deploy-config", nil))

			var body struct {
				Config map[string]any `json:"config"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
			got, ok := body.Config["grpcTlsFingerprint"]
			if !ok {
				t.Fatalf("the config carries no grpcTlsFingerprint: %s", rec.Body.String())
			}
			if got != tc.want {
				t.Errorf("grpcTlsFingerprint = %v, want %q", got, tc.want)
			}
		})
	}
}

// The whole point of resolving Redis here is that a warp spoke has no DNS into
// the cluster: handing it Core's own "redis:6379" produces a proxy that opens,
// never resolves anything, and reports nothing.
//
// The resolver is injected because the answer must not depend on the DNS of
// whatever machine runs this - CI sits behind one that answers NXDOMAIN with a
// public address of its own, which is exactly the case the private-only rule
// below exists to reject.
func TestResolveOverlayRedisAddr(t *testing.T) {
	noLookup := func(string) ([]net.IP, error) {
		t.Helper()
		t.Error("a literal address must not be resolved")
		return nil, nil
	}
	fails := func(string) ([]net.IP, error) { return nil, errors.New("no such host") }
	answers := func(ips ...string) func(string) ([]net.IP, error) {
		return func(string) ([]net.IP, error) {
			out := make([]net.IP, 0, len(ips))
			for _, s := range ips {
				out = append(out, net.ParseIP(s))
			}
			return out, nil
		}
	}

	tests := []struct {
		name   string
		in     string
		lookup func(string) ([]net.IP, error)
		want   string
	}{
		{name: "unset", in: "", lookup: noLookup, want: ""},
		{name: "whitespace only", in: "   ", lookup: noLookup, want: ""},
		{
			name:   "literal private ip keeps its port",
			in:     "10.20.0.5:6379",
			lookup: noLookup,
			want:   "10.20.0.5:6379",
		},
		{
			// Core's REDIS_ADDR is allowed to omit the port; the node needs one.
			name:   "bare ip gets the default port",
			in:     "10.20.0.5",
			lookup: noLookup,
			want:   "10.20.0.5:6379",
		},
		{
			name:   "a non-default port survives",
			in:     "10.20.0.5:6380",
			lookup: noLookup,
			want:   "10.20.0.5:6380",
		},
		{
			// The name itself is useless to a spoke, so "" is the honest answer.
			name:   "a name that does not resolve yields nothing, not the name",
			in:     "redis:6379",
			lookup: fails,
			want:   "",
		},
		{
			name:   "a service name resolves to its overlay address",
			in:     "redis:6379",
			lookup: answers("10.20.0.5"),
			want:   "10.20.0.5:6379",
		},
		{
			// A resolver that answers every name with a public landing address
			// would otherwise point every customer's proxy at a stranger.
			name:   "a hijacked NXDOMAIN answer is refused",
			in:     "redis:6379",
			lookup: answers("46.225.53.182"),
			want:   "",
		},
		{
			name:   "the private answer is preferred over a public one",
			in:     "redis:6379",
			lookup: answers("46.225.53.182", "10.20.0.5"),
			want:   "10.20.0.5:6379",
		},
		{
			// Never reachable from the machine that will dial it.
			name:   "loopback is refused",
			in:     "127.0.0.1:6379",
			lookup: noLookup,
			want:   "",
		},
		{
			// IPv6 has no path through the overlay snippets today; answering
			// with one would be a value the node cannot use.
			name:   "ipv6 is not offered",
			in:     "[fd00::1]:6379",
			lookup: noLookup,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveOverlayRedisAddr(tt.in, tt.lookup); got != tt.want {
				t.Errorf("resolveOverlayRedisAddr(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Core's gRPC address goes through the same resolver, so what has to be pinned
// here is the part that differs: the service NAME carries no port, and the port
// comes from DYLARIS_GRPC_PORT rather than a Redis default.
//
// The reason this resolves a name at all: Core used to hand out its own task IP,
// which changes on every redeploy and names one replica out of however many are
// running - while the Redis address beside it in the same snippet had always
// been a resolved service name.
func TestResolveOverlayAddrForCore(t *testing.T) {
	answers := func(ips ...string) func(string) ([]net.IP, error) {
		return func(string) ([]net.IP, error) {
			out := make([]net.IP, 0, len(ips))
			for _, s := range ips {
				out = append(out, net.ParseIP(s))
			}
			return out, nil
		}
	}
	fails := func(string) ([]net.IP, error) { return nil, errors.New("no such host") }

	tests := []struct {
		name        string
		in          string
		defaultPort string
		lookup      func(string) ([]net.IP, error)
		want        string
	}{
		{
			name:        "the service name takes the gRPC port",
			in:          "core",
			defaultPort: "25501",
			lookup:      answers("10.20.0.3"),
			want:        "10.20.0.3:25501",
		},
		{
			name:        "a non-default gRPC port is carried through",
			in:          "core",
			defaultPort: "25555",
			lookup:      answers("10.20.0.3"),
			want:        "10.20.0.3:25555",
		},
		{
			// A single-container install where "core" means nothing. The caller
			// falls back to the local address; here the honest answer is "".
			name:        "a name that does not resolve yields nothing",
			in:          "core",
			defaultPort: "25501",
			lookup:      fails,
			want:        "",
		},
		{
			// Same rule as Redis: this is what a customer's warp will dial.
			name:        "a hijacked NXDOMAIN answer is refused",
			in:          "core",
			defaultPort: "25501",
			lookup:      answers("46.225.53.182"),
			want:        "",
		},
		{
			// CORE_SERVICE_NAME is free text; an operator who writes a port in it
			// meant it, and it wins over the default.
			name:        "an explicit port in the name wins",
			in:          "core:9999",
			defaultPort: "25501",
			lookup:      answers("10.20.0.3"),
			want:        "10.20.0.3:9999",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveOverlayAddr(tt.in, tt.defaultPort, tt.lookup); got != tt.want {
				t.Errorf("resolveOverlayAddr(%q, %q) = %q, want %q", tt.in, tt.defaultPort, got, tt.want)
			}
		})
	}
}

// The warp assignment poll asks for these every 30s per peer, so they are
// resolved without the store round trip that the full snippet needs. Literal
// addresses on both sides keep DNS out of the test entirely.
func TestOverlayServiceAddrs(t *testing.T) {
	t.Setenv("REDIS_ADDR", "10.20.0.5:6379")
	t.Setenv("CORE_SERVICE_NAME", "10.20.0.4")

	core, redis := overlayServiceAddrs("25501")
	if core != "10.20.0.4:25501" {
		t.Errorf("core addr = %q, want %q", core, "10.20.0.4:25501")
	}
	if redis != "10.20.0.5:6379" {
		t.Errorf("redis addr = %q, want %q", redis, "10.20.0.5:6379")
	}
}

// A peer whose Core cannot name its own overlay must be told nothing rather than
// something plausible: warp keeps the addresses it already had, and a machine
// with hand-set values is unaffected. Empty is the honest answer, not a bug.
func TestOverlayServiceAddrsUnresolvableIsEmpty(t *testing.T) {
	t.Setenv("REDIS_ADDR", "")
	t.Setenv("CORE_SERVICE_NAME", "127.0.0.1") // loopback is never the overlay

	core, redis := overlayServiceAddrs("25501")
	if core != "" || redis != "" {
		t.Errorf("got (%q, %q), want both empty", core, redis)
	}
}

func TestGrpcPortFromEnv(t *testing.T) {
	t.Setenv("DYLARIS_GRPC_PORT", "")
	if got := grpcPortFromEnv(); got != "25501" {
		t.Errorf("default = %q, want 25501", got)
	}
	t.Setenv("DYLARIS_GRPC_PORT", " 9999 ")
	if got := grpcPortFromEnv(); got != "9999" {
		t.Errorf("override = %q, want 9999", got)
	}
}

// The enroll and assignment handlers both go through this, so the panel snippet
// and the warp client can never be handed different answers.
func TestStampOverlayAddrs(t *testing.T) {
	t.Setenv("REDIS_ADDR", "10.20.0.5:6379")
	t.Setenv("CORE_SERVICE_NAME", "10.20.0.4")
	t.Setenv("DYLARIS_GRPC_PORT", "25501")

	res := services.EnrollResult{WGIP: "10.0.99.7"}
	stampOverlayAddrs(&res)

	if res.CoreGRPCAddr != "10.20.0.4:25501" {
		t.Errorf("CoreGRPCAddr = %q", res.CoreGRPCAddr)
	}
	if res.RedisAddr != "10.20.0.5:6379" {
		t.Errorf("RedisAddr = %q", res.RedisAddr)
	}
	if res.WGIP != "10.0.99.7" {
		t.Errorf("stamping must not disturb the rest of the response, WGIP = %q", res.WGIP)
	}
}
