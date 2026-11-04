package services

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"testing"

	"dylaris-core/services/dnsprov"
)

type fakeDomainResolver struct {
	a, aaaa map[string][]netip.Addr
	srv     map[string][]*net.SRV
	// failA/failAAAA/failSRV inject a non-NXDOMAIN error (timeout, SERVFAIL).
	failA, failAAAA, failSRV bool
	failTarget               string
}

var errServFail = &net.DNSError{Err: "server misbehaving", IsTemporary: true}
var errNXDomain = &net.DNSError{Err: "no such host", IsNotFound: true}

func (f fakeDomainResolver) LookupA(_ context.Context, h string) ([]netip.Addr, error) {
	if f.failA || h == f.failTarget {
		return nil, errServFail
	}
	if v := f.a[h]; len(v) > 0 {
		return v, nil
	}
	return nil, errNXDomain
}
func (f fakeDomainResolver) LookupAAAA(_ context.Context, h string) ([]netip.Addr, error) {
	if f.failAAAA {
		return nil, errServFail
	}
	if v := f.aaaa[h]; len(v) > 0 {
		return v, nil
	}
	return nil, errNXDomain
}
func (f fakeDomainResolver) LookupSRV(_ context.Context, h string) ([]*net.SRV, error) {
	if f.failSRV {
		return nil, errServFail
	}
	if v := f.srv[h]; len(v) > 0 {
		return v, nil
	}
	return nil, errNXDomain
}

func TestEvaluateDomain(t *testing.T) {
	const fqdn = "play.example.com"
	node := netip.MustParseAddr("203.0.113.10")
	other := netip.MustParseAddr("198.51.100.7")
	v6 := netip.MustParseAddr("2001:db8::1")
	okA := map[string][]netip.Addr{fqdn: {node}}
	srvKey := "_minecraft._tcp." + fqdn

	tests := []struct {
		name     string
		r        fakeDomainResolver
		node     netip.Addr
		port     uint16
		state    string
		problems []string
	}{
		{"default port green", fakeDomainResolver{a: okA}, node, 25565, "green", nil},
		{"custom port without SRV", fakeDomainResolver{a: okA}, node, 25650, "yellow",
			[]string{"Players would have to type :25650 - add an SRV record _minecraft._tcp.play.example.com pointing to play.example.com port 25650"}},
		{"custom port with good SRV", fakeDomainResolver{a: okA,
			srv: map[string][]*net.SRV{srvKey: {{Target: "play.example.com.", Port: 25650}}}}, node, 25650, "green", nil},
		{"SRV wrong port", fakeDomainResolver{a: okA,
			srv: map[string][]*net.SRV{srvKey: {{Target: "play.example.com.", Port: 25565}}}}, node, 25650, "yellow",
			[]string{"SRV points to port 25565, should be 25650"}},
		{"SRV target resolves elsewhere", fakeDomainResolver{a: okA,
			srv: map[string][]*net.SRV{srvKey: {{Target: "other.example.com.", Port: 25650}}},
		}, node, 25650, "yellow", []string{"SRV target other.example.com does not resolve to 203.0.113.10"}},
		{"default port but stray SRV", fakeDomainResolver{a: okA,
			srv: map[string][]*net.SRV{srvKey: {{Target: "play.example.com.", Port: 25650}}}}, node, 25565, "yellow",
			[]string{"SRV points to port 25650, should be 25565"}},
		{"A wrong", fakeDomainResolver{a: map[string][]netip.Addr{fqdn: {other}}}, node, 25565, "yellow",
			[]string{"points to 198.51.100.7, should be 203.0.113.10"}},
		{"AAAA present", fakeDomainResolver{a: okA, aaaa: map[string][]netip.Addr{fqdn: {v6}}}, node, 25565, "yellow",
			[]string{"IPv6 players reach a different host (AAAA 2001:db8::1)"}},
		{"A wrong and AAAA, A first", fakeDomainResolver{a: map[string][]netip.Addr{fqdn: {other}}, aaaa: map[string][]netip.Addr{fqdn: {v6}}}, node, 25565, "yellow",
			[]string{"points to 198.51.100.7, should be 203.0.113.10", "IPv6 players reach a different host (AAAA 2001:db8::1)"}},
		{"default port SRV to foreign target", fakeDomainResolver{a: map[string][]netip.Addr{fqdn: {node}, "evil.example": {other}},
			srv: map[string][]*net.SRV{srvKey: {{Target: "evil.example.", Port: 25565}}}}, node, 25565, "yellow",
			[]string{"SRV target evil.example does not resolve to 203.0.113.10"}},
		{"custom port, second SRV foreign", fakeDomainResolver{a: okA,
			srv: map[string][]*net.SRV{srvKey: {{Target: "play.example.com.", Port: 25650}, {Target: "evil.example.", Port: 25650}}}},
			node, 25650, "yellow", []string{"SRV also points to evil.example:25650"}},
		{"custom port, second SRV other port", fakeDomainResolver{a: okA,
			srv: map[string][]*net.SRV{srvKey: {{Target: "play.example.com.", Port: 25650}, {Target: "play.example.com.", Port: 25565}}}},
			node, 25650, "yellow", []string{"SRV also points to play.example.com:25565"}},
		{"SRV target node plus extra", fakeDomainResolver{a: map[string][]netip.Addr{fqdn: {node}, "t.example.com": {node, other}},
			srv: map[string][]*net.SRV{srvKey: {{Target: "t.example.com.", Port: 25650}}}}, node, 25650, "yellow",
			[]string{"SRV target t.example.com also points to 198.51.100.7, should only be 203.0.113.10"}},
		{"SRV target lookup failed", fakeDomainResolver{a: okA, failTarget: "t.example.com",
			srv: map[string][]*net.SRV{srvKey: {{Target: "t.example.com.", Port: 25650}}}}, node, 25650, "grey",
			[]string{"DNS lookup failed (SRV target A), checking again shortly"}},
		{"A has node plus another", fakeDomainResolver{a: map[string][]netip.Addr{fqdn: {node, other}}}, node, 25565, "yellow",
			[]string{"also points to 198.51.100.7, should only be 203.0.113.10"}},
		{"AAAA lookup failed", fakeDomainResolver{a: okA, failAAAA: true}, node, 25565, "grey",
			[]string{"DNS lookup failed (AAAA), checking again shortly"}},
		{"SRV lookup failed", fakeDomainResolver{a: okA, failSRV: true}, node, 25650, "grey",
			[]string{"DNS lookup failed (SRV), checking again shortly"}},
		{"A lookup failed", fakeDomainResolver{failA: true}, node, 25565, "grey",
			[]string{"DNS lookup failed (A), checking again shortly"}},
		{"no A", fakeDomainResolver{}, node, 25565, "grey", []string{"does not resolve yet"}},
		{"node IP unknown", fakeDomainResolver{a: okA}, netip.Addr{}, 25565, "grey", []string{"the node has not reported a public IP yet"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateDomain(context.Background(), tc.r, fqdn, tc.node, tc.port)
			if got.State != tc.state || !reflect.DeepEqual(got.Problems, tc.problems) {
				t.Fatalf("got %+v, want state %s problems %q", got, tc.state, tc.problems)
			}
		})
	}
}

func TestExpectedRecordsUnmapsIPv4(t *testing.T) {
	got := ExpectedRecords("s", "example.com", netip.MustParseAddr("::ffff:203.0.113.10"), 25565)
	if len(got) != 1 || got[0].Value != "203.0.113.10" {
		t.Fatalf("got %+v", got)
	}
}

func TestExpectedRecords(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.10")
	a := dnsprov.Record{Name: "survival", Type: "A", Value: "203.0.113.10"}
	if got := ExpectedRecords("survival", "example.com", ip, 25565); !reflect.DeepEqual(got, []dnsprov.Record{a}) {
		t.Fatalf("25565: %+v", got)
	}
	want := []dnsprov.Record{a, {Name: "_minecraft._tcp.survival", Type: "SRV", Value: "survival.example.com", Port: 25650}}
	if got := ExpectedRecords("survival", "example.com", ip, 25650); !reflect.DeepEqual(got, want) {
		t.Fatalf("25650: %+v", got)
	}
}
