package netguard

import (
	"net"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestAllowed(t *testing.T) {
	mustParse := func(s string) []netip.Prefix {
		t.Helper()
		p, err := ParseAllowList(s)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	one := mustParse("192.168.1.10/32")
	everything := mustParse("0.0.0.0/0,::/0")
	for _, tc := range []struct {
		ip    string
		allow []netip.Prefix
		want  bool
	}{
		{"192.168.1.10", one, true},
		{"::ffff:192.168.1.10", one, true},
		{"192.168.1.11", one, false},
		{"10.0.0.1", one, false},
		{"8.8.8.8", one, true},
		{"8.8.8.8", nil, true},
		{"192.168.1.10", nil, false},
		{"10.0.5.7", everything, true},
		{"fd00::1", everything, true},
		// Never, whatever is listed.
		{"127.0.0.1", everything, false},
		{"169.254.169.254", everything, false},
		{"0.0.0.0", everything, false},
		{"224.0.0.1", everything, false},
		{"::1", everything, false},
		{"fe80::1", everything, false},
		{"::", everything, false},
		{"ff02::1", everything, false},
		{"ff01::1", everything, false}, // interface-local multicast
		{"127.0.0.1", mustParse("127.0.0.0/8"), false},
		{"169.254.169.254", mustParse("169.254.169.254/32"), false},
	} {
		if got := Allowed(net.ParseIP(tc.ip), tc.allow); got != tc.want {
			t.Errorf("Allowed(%s, %v) = %v, want %v", tc.ip, tc.allow, got, tc.want)
		}
	}
}

// With nothing listed, Allowed is exactly !Disallowed: an empty setting is
// today's behaviour.
func TestAllowedEmptyMatchesDisallowed(t *testing.T) {
	for _, s := range []string{"1.1.1.1", "10.0.0.1", "100.64.0.1", "172.16.0.1", "127.0.0.1", "169.254.169.254", "fd00::1", "2001:db8::1", "::1"} {
		ip := net.ParseIP(s)
		if Allowed(ip, nil) == Disallowed(ip) {
			t.Errorf("%s: Allowed(nil) = %v, Disallowed = %v", s, Allowed(ip, nil), Disallowed(ip))
		}
	}
}

func TestParseAllowList(t *testing.T) {
	got, err := ParseAllowList(" 192.168.1.10/32 , ,10.0.5.9/24,fd00::/8 ")
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("192.168.1.10/32"), netip.MustParsePrefix("10.0.5.0/24"), netip.MustParsePrefix("fd00::/8")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, err := ParseAllowList(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"192.168.1.10", "10.0.0.0/33", "nope"} {
		_, err := ParseAllowList("10.0.0.0/8," + bad)
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("%q: err = %v, want one naming the value", bad, err)
		}
	}
}

// The allow list widens TenantDialer only; Dialer (the import fetcher) keeps
// refusing.
func TestPrivateAllowWidensOnlyTheTenantDialer(t *testing.T) {
	SetPrivateAllow([]netip.Prefix{netip.MustParsePrefix("10.1.2.3/32")})
	t.Cleanup(func() { SetPrivateAllow(nil) })
	if err := TenantDialer.Control("tcp", "10.1.2.3:9000", nil); err != nil {
		t.Errorf("tenant dialer refused a listed address: %v", err)
	}
	if err := TenantDialer.Control("tcp", "10.1.2.4:9000", nil); err == nil {
		t.Error("tenant dialer let an unlisted private address through")
	}
	if err := Dialer.Control("tcp", "10.1.2.3:9000", nil); err == nil {
		t.Error("the shared Dialer was widened by the storage allow list")
	}
	if got := PrivateAllow(); !reflect.DeepEqual(got, []string{"10.1.2.3/32"}) {
		t.Errorf("PrivateAllow() = %v", got)
	}
	SetPrivateAllow(nil)
	if got := PrivateAllow(); got != nil {
		t.Errorf("PrivateAllow() after clearing = %v, want nil", got)
	}
}
