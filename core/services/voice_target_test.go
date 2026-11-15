package services

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"

	"dylaris-core/store"
)

// TestVoiceTargetFor is the hosted voice target rule: a regional address is
// today's answer with no DNS; a server that already has voice keeps its row
// with no DNS; only then are its own domains asked, in lexical order, and an
// unknown answer ends the walk.
func TestVoiceTargetFor(t *testing.T) {
	const uuid = "srv-1"
	route := func(d string) GatewayRoute { return GatewayRoute{Domain: d, ServerUUID: uuid} }
	const ourName = "voice.eu.dylaris.com"
	prior := &store.VoicePort{ServerID: 1, Region: "us", Port: 26005, Host: "voice.us.dylaris.com"}
	pointsAt := func(name string, ip netip.Addr) func(*fakePointingResolver) {
		return func(f *fakePointingResolver) { f.a[name] = []netip.Addr{ip} }
	}
	cases := []struct {
		name       string
		routes     []GatewayRoute
		prior      *store.VoicePort
		dns        []func(*fakePointingResolver)
		wantHost   string // on a refusal: the name it is about
		wantRegion string
		wantStatus PointingStatus
		wantOK     bool
		zeroCalls  bool
		notAsked   string // a name the resolver must never see
	}{
		{"regional only", []GatewayRoute{route("a.eu.dylaris.com")}, nil, nil,
			"a.eu.dylaris.com", "eu", "", true, true, ""},
		{"regional beside own domains", []GatewayRoute{route("aaa.example.com"), route("z.eu.dylaris.com"), route("play.example.com")}, nil,
			[]func(*fakePointingResolver){pointsAt("aaa.example.com", ptUS)},
			"z.eu.dylaris.com", "eu", "", true, true, ""},
		{"regional wins over an existing row", []GatewayRoute{route("a.eu.dylaris.com"), route("play.example.com")}, prior, nil,
			"a.eu.dylaris.com", "eu", "", true, true, ""},
		{"own domain only, points at eu", []GatewayRoute{route("play.example.com")}, nil,
			[]func(*fakePointingResolver){pointsAt("play.example.com", ptEU1)},
			ourName, "eu", PointingOK, true, false, ""},
		{"first elsewhere, second ok", []GatewayRoute{route("b.example.com"), route("a.example.com")}, nil,
			[]func(*fakePointingResolver){pointsAt("a.example.com", ptForeign), pointsAt("b.example.com", ptUS)},
			"voice.us.dylaris.com", "us", PointingOK, true, false, ""},
		{"both refused: the first name's refusal", []GatewayRoute{route("b.example.com"), route("a.example.com")}, nil,
			[]func(*fakePointingResolver){pointsAt("a.example.com", ptForeign)},
			"a.example.com", "", PointingElsewhere, false, false, ""},
		{"first lookup failed: the second is never asked", []GatewayRoute{route("a.example.com"), route("b.example.com")}, nil,
			[]func(*fakePointingResolver){
				func(f *fakePointingResolver) {
					f.fail["CNAME a.example.com"] = &net.DNSError{Err: "timeout", IsTimeout: true}
				},
				pointsAt("b.example.com", ptEU1)},
			"a.example.com", "", PointingLookupFailed, false, false, "b.example.com"},
		{"existing row on an own domain: reused, no DNS", []GatewayRoute{route("play.example.com")}, prior,
			[]func(*fakePointingResolver){pointsAt("play.example.com", ptEU1)},
			"voice.us.dylaris.com", "us", "", true, true, ""},
		{"existing row, no route left", nil, prior, nil, "", "", "", false, true, ""},
		{"no routes", nil, nil, nil, "", "", "", false, true, ""},
		{"another server's own domain is never asked", []GatewayRoute{{Domain: "play.example.com", ServerUUID: "other"}}, nil,
			[]func(*fakePointingResolver){pointsAt("play.example.com", ptEU1)},
			"", "", "", false, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := newFakePointingResolver()
			for _, f := range c.dns {
				f(res)
			}
			pc, _ := newTestPointing(res, ptEdges())
			host, region, p, ok := VoiceTargetFor(context.Background(), pc, "owner-1", c.routes, ptEdges(), uuid, c.prior, nil)
			if host != c.wantHost || region != c.wantRegion || p.Status != c.wantStatus || ok != c.wantOK {
				t.Fatalf("got (%q, %q, %q, %v), want (%q, %q, %q, %v)", host, region, p.Status, ok, c.wantHost, c.wantRegion, c.wantStatus, c.wantOK)
			}
			if c.zeroCalls && res.total() != 0 {
				t.Fatalf("%d resolver calls, want none", res.total())
			}
			if c.notAsked != "" && res.names[c.notAsked] {
				t.Fatalf("%s was asked after an unknown answer", c.notAsked)
			}
		})
	}
}

// A rate-limited first name ends the walk like a failed lookup.
func TestVoiceTargetForRateLimitedEndsTheWalk(t *testing.T) {
	res := newFakePointingResolver()
	res.a["b.example.com"] = []netip.Addr{ptEU1}
	pc, _ := newTestPointing(res, ptEdges())
	routes := []GatewayRoute{{Domain: "a.example.com", ServerUUID: "srv-1"}, {Domain: "b.example.com", ServerUUID: "srv-1"}}
	host, _, p, ok := VoiceTargetFor(context.Background(), pc, "owner-1", routes, ptEdges(), "srv-1", nil, func() bool { return false })
	if ok || p.Status != PointingRateLimited || host != "a.example.com" || res.total() != 0 {
		t.Fatalf("got %q %+v %v, %d calls", host, p, ok, res.total())
	}
}

// A nil checker is "could not check", never a refusal.
func TestVoiceTargetForNilCheckerIsUnknown(t *testing.T) {
	routes := []GatewayRoute{{Domain: "play.example.com", ServerUUID: "srv-1"}}
	_, _, p, ok := VoiceTargetFor(context.Background(), nil, "owner-1", routes, ptEdges(), "srv-1", nil, nil)
	if ok || p.Known() {
		t.Fatalf("got %+v %v, want unknown", p, ok)
	}
}

// The reconcile republishes an own-domain row with OUR name as it is: it
// never resolves (it holds no checker), so a later DNS change cannot move it.
func TestReconcileKeepsOwnDomainVoiceHost(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	p := testVoicePort()
	p.Host = "voice.eu.dylaris.com"
	fs := &voiceFakeStore{rows: []store.VoicePort{p}}
	v := NewVoiceService(fs, rdb)
	if err := v.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := mr.Get(ServerVoiceKey(p.ServerUUID))
	if want := `"host":"voice.eu.dylaris.com"`; !strings.Contains(got, want) {
		t.Fatalf("server key %s, want %s", got, want)
	}
}
