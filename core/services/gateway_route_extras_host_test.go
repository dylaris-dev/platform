package services

import (
	"context"
	"strings"
	"testing"

	"dylaris-core/services/remotegw"
)

func TestVoiceAddressFor(t *testing.T) {
	regions := []remotegw.Region{
		{Region: "eu", Wildcard: "*.eu.example.com", Online: true},
		{Region: "us", Wildcard: "*.us.example.com", Online: false},
	}
	for _, c := range []struct {
		name    string
		domains []string
		own     map[string]bool
		want    string
		ok      bool
	}{
		{"regional only", []string{"a.eu.example.com"}, nil, "a.eu.example.com", true},
		{"regional beats an own domain", []string{"aaa.acme.tld", "z.eu.example.com"}, map[string]bool{"aaa.acme.tld": true}, "z.eu.example.com", true},
		{"own domain only", []string{"play.acme.tld"}, map[string]bool{"play.acme.tld": true}, "play.acme.tld", true},
		{"lexically first own domain", []string{"z.acme.tld", "b.acme.tld"}, map[string]bool{"z.acme.tld": true, "b.acme.tld": true}, "b.acme.tld", true},
		{"two labels deep is neither", []string{"x.a.eu.example.com"}, nil, "", false},
		{"offline region and no own domain", []string{"a.us.example.com"}, nil, "", false},
		{"offline region beside an own domain", []string{"a.us.example.com", "play.acme.tld"}, map[string]bool{"play.acme.tld": true}, "play.acme.tld", true},
		{"none", nil, nil, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := VoiceAddressFor(c.domains, c.own, regions)
			if got != c.want || ok != c.ok {
				t.Fatalf("VoiceAddressFor(%v) = %q %v, want %q %v", c.domains, got, ok, c.want, c.ok)
			}
		})
	}
}

const ownVoiceName = "voice.eu.example.com"

// addOwnServer is addServer on the customer's own domain.
func (e *extrasEnv) addOwnServer(i int) string {
	_, d := e.addServer(i)
	e.st.mu.Lock()
	r := e.st.routes[d]
	r.CustomDomain = true
	e.st.routes[d] = r
	e.st.mu.Unlock()
	return d
}

func (e *extrasEnv) setVoiceHost(domain, host string) {
	e.st.mu.Lock()
	defer e.st.mu.Unlock()
	r := e.st.routes[domain]
	r.VoiceHost = host
	e.st.routes[domain] = r
}

// The provider's host is stored for an own domain and read back; a regional
// address stores nothing new, so its row stays what it is today.
func TestEnableServerVoiceStoresTheProviderHost(t *testing.T) {
	e := newExtrasEnv(t)
	g := NewRemoteGateway(e.st, func() *remotegw.Client { return e.c })
	srvReg, reg := e.addServer(1)
	own := e.addOwnServer(2)
	e.fake.SetVoiceHost(own, ownVoiceName)
	srvOwn := e.st.servers[102]

	port, host, err := g.EnableServerVoice(context.Background(), srvOwn.UUID, own, "svc")
	if err != nil || host != ownVoiceName || port <= 0 {
		t.Fatalf("own domain: %d %q %v", port, host, err)
	}
	if r := e.row(own); r.VoiceHost != ownVoiceName || r.VoicePort == nil || *r.VoicePort != port {
		t.Fatalf("own row %+v", r)
	}
	if h, p, _, on, err := g.ServerVoice(srvOwn.UUID); err != nil || !on || h != ownVoiceName || p != port {
		t.Fatalf("ServerVoice own = %q %d %v %v", h, p, on, err)
	}

	_, host, err = g.EnableServerVoice(context.Background(), srvReg.UUID, reg, "svc")
	if err != nil || host != reg {
		t.Fatalf("regional: %q %v", host, err)
	}
	if r := e.row(reg); r.VoiceHost != "" {
		t.Fatalf("regional row stored host %q, want empty as before", r.VoiceHost)
	}
	if h, _, _, _, _ := g.ServerVoice(srvReg.UUID); h != reg {
		t.Fatalf("ServerVoice regional = %q", h)
	}
}

// An older replica switching voice off leaves voice_host behind: it is read
// only while a port is set.
func TestServerVoiceIgnoresAHostWithoutAPort(t *testing.T) {
	e := newExtrasEnv(t)
	g := NewRemoteGateway(e.st, func() *remotegw.Client { return e.c })
	srv, d := e.addServer(1)
	e.setVoiceHost(d, ownVoiceName)
	if _, _, _, on, err := g.ServerVoice(srv.UUID); on || err != nil {
		t.Fatalf("voice reads on without a port: %v %v", on, err)
	}
}

// Review Focus 5: the codes parts 1, 2 and 3 add are about the moment, never final.
// (The sweep tests use addServer: an own-domain row also passes the claim
// check, which is not what these pin, and the host rule does not read it.)
// A re-assert answered with any of them, twice a sweep apart, changes nothing.
func TestReconcileRouteExtrasNewCodesAreNotFinal(t *testing.T) {
	for _, c := range []struct{ code, msg string }{
		{"no_routes", "Voice chat needs to know which region this server's players arrive in."},
		{"dns_lookup_failed", "The lookup of the address failed right now. Nothing was changed. Try again in a moment."},
		{"rate_limited", "Too many DNS checks. Wait a minute."},
		// Part 3: the white-label name's codes.
		{"voice_host_invalid", "voice.other.tld has to be the address or a name under it."},
		{"voice_host_not_pointing", "voice.acme.tld does not point at the gateway yet."},
	} {
		t.Run(c.code, func(t *testing.T) {
			e := newExtrasEnv(t)
			_, d := e.addServer(1)
			e.fake.SetVoiceHost(d, ownVoiceName)
			port := e.voiceOn(t, d)
			e.setVoiceHost(d, ownVoiceName)
			// The provider lost the entry; its new one would need DNS again.
			e.fake.DropRouteExtras(d)
			e.fake.SetVoiceDNS(d, c.code, c.msg)
			before := len(e.fake.Calls())
			for i := 0; i < 3; i++ {
				e.sweep(t)
				e.later(outboxSweepEvery)
			}
			if n := extrasWrites(e.fake.Calls()[before:]); n < 2 {
				t.Fatalf("%d re-asserts in three sweeps; the code was never answered twice", n)
			}
			if r := e.row(d); r.VoicePort == nil || *r.VoicePort != port || r.VoiceHost != ownVoiceName {
				t.Fatalf("row %+v, want port %d host %s", r, port, ownVoiceName)
			}
			if ops := openExtrasOps(e.st); len(ops) != 0 {
				t.Fatalf("queued %v", ops)
			}
			if strings.Contains(e.logs.String(), "switch it on again") {
				t.Fatalf("a switch-off was logged:\n%s", e.logs.String())
			}
		})
	}
	for _, code := range []string{"no_routes", "dns_lookup_failed", "rate_limited", "voice_host_invalid", "voice_host_not_pointing"} {
		if voiceRefusalIsFinal(&remotegw.APIError{Status: 409, Code: code}) {
			t.Fatalf("%s reads as final", code)
		}
	}

	// Bedrock (part 2): the provider's add check of an own domain answers
	// these when its entry was lost and the re-assert has to add it again.
	for _, c := range []struct{ code, msg string }{
		{"bedrock_unavailable", "play.acme.tld points to 192.0.2.50, not to the gateway. Add a CNAME from play.acme.tld to the address shown under Your own domains."},
		{"dns_lookup_failed", "The lookup of the address failed right now. Nothing was changed. Try again in a moment."},
		{"rate_limited", "Too many DNS checks. Wait a minute."},
	} {
		t.Run("Bedrock "+c.code, func(t *testing.T) {
			e := newExtrasEnv(t)
			srv, d := e.addServer(1)
			key := e.bedrockOn(t, srv, d, true)
			e.fake.DropRouteExtras(d)
			e.fake.SetBedrockDNS(d, c.code, c.msg)
			before := len(e.fake.Calls())
			for i := 0; i < 3; i++ {
				e.sweep(t)
				e.later(outboxSweepEvery)
			}
			if n := extrasWrites(e.fake.Calls()[before:]); n < 2 {
				t.Fatalf("%d re-asserts in three sweeps; the code was never answered twice", n)
			}
			if r := e.row(d); r.Edition != "both" {
				t.Fatalf("edition %q, want both", r.Edition)
			}
			if got := e.st.keys[srv.ID]; string(got) != string(key) {
				t.Fatalf("stored key changed: %s, want %s", extrasKeyID(got), extrasKeyID(key))
			}
			if ops := openExtrasOps(e.st); len(ops) != 0 {
				t.Fatalf("queued %v", ops)
			}
			if strings.Contains(e.logs.String(), "switch it on again") {
				t.Fatalf("a switch-off was logged:\n%s", e.logs.String())
			}
		})
	}
	for _, code := range []string{"bedrock_unavailable", "dns_lookup_failed", "rate_limited"} {
		if bedrockRefusalIsFinal(&remotegw.APIError{Status: 409, Code: code}) {
			t.Fatalf("Bedrock %s reads as final", code)
		}
	}
}

// A host here that differs from the provider's, with the same port, is left
// alone: the sweep never writes the host (a write racing a voice-off on
// another replica could undo it), and it is never a reason to switch off.
func TestReconcileRouteExtrasLeavesTheStoredHostAlone(t *testing.T) {
	e := newExtrasEnv(t)
	_, d := e.addServer(1)
	e.fake.SetVoiceHost(d, ownVoiceName)
	port := e.voiceOn(t, d)
	e.setVoiceHost(d, "voice.old.example.com")
	for i := 0; i < 2; i++ {
		e.sweep(t)
		e.later(outboxSweepEvery)
	}
	r := e.row(d)
	if r.VoicePort == nil || *r.VoicePort != port || r.VoiceHost != "voice.old.example.com" {
		t.Fatalf("row %+v, want port %d host voice.old.example.com", r, port)
	}
	if ops := openExtrasOps(e.st); len(ops) != 0 {
		t.Fatalf("queued %v", ops)
	}
	if logs := e.logs.String(); strings.Contains(logs, "switch it on again") || strings.Contains(logs, ownVoiceName) {
		t.Fatalf("want no switch-off and no host notice:\n%s", logs)
	}
}

// A provider that lists no host (older than part 1) says nothing about it:
// the stored host stays.
func TestReconcileRouteExtrasKeepsTheHostOnAnOldProvider(t *testing.T) {
	e := newExtrasEnv(t)
	_, d := e.addServer(1)
	port := e.voiceOn(t, d)
	e.setVoiceHost(d, ownVoiceName)
	srv := *e.st.servers[101]
	budget := routeExtrasPerSweep
	p := remotegw.Route{Domain: d, LinkID: e.kit.LinkID, Voice: &remotegw.RouteVoice{Enabled: true, Port: port, TargetPort: VoiceInternalPort}}
	if err := reconcileRouteExtras(context.Background(), e.st, e.c, e.publish, nil, e.row(d), p, srv, &budget); err != nil {
		t.Fatal(err)
	}
	if r := e.row(d); r.VoiceHost != ownVoiceName || r.VoicePort == nil || *r.VoicePort != port {
		t.Fatalf("row %+v", r)
	}
}
