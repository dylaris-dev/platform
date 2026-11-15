package services

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services/remotegw"
)

type voiceRewrite struct {
	serverID   int
	from, host string
	port       int
}

// rewrites records the sweep's voice mod writes; fail makes them fail, as a
// node that is down.
type rewrites struct {
	mu   sync.Mutex
	got  []voiceRewrite
	fail bool
}

func (r *rewrites) write(_ context.Context, srv *models.Server, from, host string, port int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, voiceRewrite{srv.ID, from, host, port})
	if r.fail {
		return errors.New("node offline")
	}
	return nil
}

func (r *rewrites) all() []voiceRewrite {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]voiceRewrite(nil), r.got...)
}

func (e *extrasEnv) setVoicePublicHost(domain, host string) {
	e.st.mu.Lock()
	defer e.st.mu.Unlock()
	r := e.st.routes[domain]
	r.VoicePublicHost = host
	e.st.routes[domain] = r
}

// Review Focus 6, remote half: the provider's background re-check fell back.
// One sweep updates the column once, starts one background config write with
// the provider's host and logs one line with the restart instruction; the
// next sweep does nothing. Port, outbox and the provider are left alone.
func TestRemoteSweepPropagatesFallBack(t *testing.T) {
	e := newExtrasEnv(t)
	rw := &rewrites{}
	e.rewrite = rw.write
	srv, d := e.addServer(1)
	name := "voice." + d
	e.fake.SetVoiceHost(d, ownVoiceName)
	port := e.voiceOn(t, d)
	e.setVoiceHost(d, ownVoiceName)
	if v, err := e.c.SetVoiceName(context.Background(), d, name); err != nil || v.ModHost != name {
		t.Fatalf("name on the provider: %+v %v", v, err)
	}
	e.setVoicePublicHost(d, name)

	// Agreeing: nothing.
	e.later(outboxSweepEvery)
	e.sweep(t)
	if got := rw.all(); len(got) != 0 || e.row(d).VoicePublicHost != name {
		t.Fatalf("an active name was touched: rewrites %v row %+v", got, e.row(d))
	}

	e.fake.FallBack(d)
	before := len(e.fake.Calls())
	e.later(outboxSweepEvery)
	e.sweep(t)
	if r := e.row(d); r.VoicePublicHost != "" || r.VoiceHost != ownVoiceName || r.VoicePort == nil || *r.VoicePort != port {
		t.Fatalf("row after the fall back %+v", r)
	}
	want := voiceRewrite{srv.ID, name, ownVoiceName, port}
	if got := rw.all(); len(got) != 1 || got[0] != want {
		t.Fatalf("rewrites %v, want [%v]", got, want)
	}
	line := "voice chat of " + d + ": the name " + name + " no longer points at the gateway, so the voice mod was switched back to " + ownVoiceName + ". Restart the server to apply it."
	if n := strings.Count(e.logs.String(), line); n != 1 {
		t.Fatalf("%d log lines %q in:\n%s", n, line, e.logs.String())
	}
	if n := extrasWrites(e.fake.Calls()[before:]); n != 0 {
		t.Fatalf("%d provider writes, want 0", n)
	}
	if ops := openExtrasOps(e.st); len(ops) != 0 {
		t.Fatalf("queued %v", ops)
	}

	e.later(outboxSweepEvery)
	e.sweep(t)
	if got := rw.all(); len(got) != 1 || strings.Count(e.logs.String(), line) != 1 {
		t.Fatalf("the next sweep acted again: rewrites %v logs:\n%s", got, e.logs.String())
	}
}

// Review Focus 7 and the plan's "never towards the customer's name in the
// background": a provider without mod_host (older than part 3) changes
// nothing, nor does one naming a white-label name this row does not carry.
func TestRemoteSweepIgnoresModHostOnAnOldProvider(t *testing.T) {
	for _, c := range []struct {
		name, local string
		voice       remotegw.RouteVoice
	}{
		{"part 1 provider, a name here", "voice.s01.example.com", remotegw.RouteVoice{Host: ownVoiceName}},
		{"7b-2 provider, a name here", "voice.s01.example.com", remotegw.RouteVoice{}},
		{"provider names a name this row lacks", "", remotegw.RouteVoice{Host: ownVoiceName, ModHost: "voice.s01.example.com", PublicHost: "voice.s01.example.com", PublicState: "active"}},
		{"provider names another name", "voice.s01.example.com", remotegw.RouteVoice{Host: ownVoiceName, ModHost: "v2.s01.example.com", PublicHost: "v2.s01.example.com", PublicState: "active"}},
		// Review I2: "our host" is the one stored here, never one the same
		// answer names.
		{"provider names a host of its own", "voice.s01.example.com", remotegw.RouteVoice{Host: "x.tld", ModHost: "x.tld", PublicHost: "voice.s01.example.com", PublicState: "fallen_back"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newExtrasEnv(t)
			rw := &rewrites{}
			_, d := e.addServer(1)
			e.fake.SetVoiceHost(d, ownVoiceName)
			port := e.voiceOn(t, d)
			e.setVoiceHost(d, ownVoiceName)
			e.setVoicePublicHost(d, c.local)
			e.later(outboxSweepEvery)
			srv := *e.st.servers[101]
			budget := routeExtrasPerSweep
			v := c.voice
			v.Enabled, v.Port, v.TargetPort = true, port, VoiceInternalPort
			p := remotegw.Route{Domain: d, LinkID: e.kit.LinkID, Voice: &v}
			if err := reconcileRouteExtras(context.Background(), e.st, e.c, e.publish, rw.write, e.row(d), p, srv, &budget); err != nil {
				t.Fatal(err)
			}
			if r := e.row(d); r.VoicePublicHost != c.local || r.VoicePort == nil || *r.VoicePort != port {
				t.Fatalf("row %+v", r)
			}
			if got := rw.all(); len(got) != 0 {
				t.Fatalf("rewrites %v", got)
			}
		})
	}
}

// Turning voice on again keeps the white-label name, on the provider and in
// the column, so the mod is told it again rather than the provider's host.
func TestEnableServerVoiceKeepsTheWhiteLabelName(t *testing.T) {
	e := newExtrasEnv(t)
	g := NewRemoteGateway(e.st, func() *remotegw.Client { return e.c })
	srv, d := e.addServer(1)
	e.fake.SetVoiceHost(d, ownVoiceName)
	if _, _, err := g.EnableServerVoice(context.Background(), srv.UUID, d, "svc"); err != nil {
		t.Fatal(err)
	}
	e.setVoicePublicHost(d, "voice."+d)
	e.later(outboxSweepEvery)
	if _, host, err := g.EnableServerVoice(context.Background(), srv.UUID, d, "svc"); err != nil || host != "voice."+d {
		t.Fatalf("re-enable told the mod %q (%v), want the white-label name", host, err)
	}
	if h, _, _, _, _ := g.ServerVoice(srv.UUID); h != "voice."+d {
		t.Fatalf("ServerVoice = %q", h)
	}
}

// Review I1: remote mode has no start sync, so a fall back whose config write
// did not land keeps the column, logs nothing on the Errors screen, and the
// next sweep writes again; the one line comes after the write that landed.
func TestRemoteSweepRetriesAFailedFallBackWrite(t *testing.T) {
	e := newExtrasEnv(t)
	rw := &rewrites{fail: true}
	e.rewrite = rw.write
	_, d := e.addServer(1)
	name := "voice." + d
	e.fake.SetVoiceHost(d, ownVoiceName)
	e.voiceOn(t, d)
	e.setVoiceHost(d, ownVoiceName)
	if _, err := e.c.SetVoiceName(context.Background(), d, name); err != nil {
		t.Fatal(err)
	}
	e.setVoicePublicHost(d, name)
	e.fake.FallBack(d)
	const line = "so the voice mod was switched back to"
	for i := 1; i <= 2; i++ {
		e.later(outboxSweepEvery)
		e.sweep(t)
		if got := rw.all(); len(got) != i || e.row(d).VoicePublicHost != name || strings.Contains(e.logs.String(), line) {
			t.Fatalf("failed sweep %d: rewrites %v row %+v logs:\n%s", i, got, e.row(d), e.logs.String())
		}
	}
	rw.mu.Lock()
	rw.fail = false
	rw.mu.Unlock()
	e.later(outboxSweepEvery)
	e.sweep(t)
	if got := rw.all(); len(got) != 3 || e.row(d).VoicePublicHost != "" || strings.Count(e.logs.String(), line) != 1 {
		t.Fatalf("after the node is back: rewrites %v row %+v logs:\n%s", got, e.row(d), e.logs.String())
	}
}

// The voice address's white-label state for the view: the provider's, from
// its cached route list; the column only when the list cannot be read or
// the provider is older.
func TestServerVoiceName(t *testing.T) {
	for _, c := range []struct {
		name, local, state string
		old, unreadable    bool
		want               RemoteVoiceName
		// target: the provider's name_target passed through, "-" for none
		// (the column fallback knows no target).
		target string
	}{
		{"pending on the provider", "", "pending", false, false, RemoteVoiceName{OurHost: ownVoiceName, PublicHost: "voice.s01.example.com", PublicState: "pending"}, ownVoiceName},
		{"active on the provider", "voice.s01.example.com", "", false, false, RemoteVoiceName{OurHost: ownVoiceName, PublicHost: "voice.s01.example.com", PublicState: "active"}, ownVoiceName},
		{"fallen back on the provider", "voice.s01.example.com", "fallen_back", false, false, RemoteVoiceName{OurHost: ownVoiceName, PublicHost: "voice.s01.example.com", PublicState: "fallen_back"}, ownVoiceName},
		{"list unreadable", "voice.s01.example.com", "", false, true, RemoteVoiceName{OurHost: ownVoiceName, PublicHost: "voice.s01.example.com", PublicState: "active"}, "-"},
		{"older provider", "voice.s01.example.com", "", true, false, RemoteVoiceName{OurHost: ownVoiceName, PublicHost: "voice.s01.example.com", PublicState: "active"}, "-"},
		{"no name", "", "none", false, false, RemoteVoiceName{OurHost: ownVoiceName}, ownVoiceName},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newExtrasEnv(t)
			srv, d := e.addServer(1)
			e.fake.SetVoiceHost(d, ownVoiceName)
			e.voiceOn(t, d)
			e.setVoiceHost(d, ownVoiceName)
			if c.state != "none" {
				e.fake.SetVoiceNameState(d, c.state)
				_, _ = e.c.SetVoiceName(context.Background(), d, "voice."+d)
			}
			if c.state == "fallen_back" {
				e.fake.SetVoiceNameState(d, "")
				_, _ = e.c.SetVoiceName(context.Background(), d, "voice."+d)
				e.fake.FallBack(d)
				e.fake.SetVoiceNameView(d, "", "2026-11-15T10:00:00Z")
			}
			e.setVoicePublicHost(d, c.local)
			api := e.c
			if c.unreadable {
				api = remotegw.New("http://127.0.0.1:1", func() string { return "k" })
			}
			if c.old {
				api = remotegw.New(oldVoiceProvider(t, e.fake.URL()), e.fake.Key)
			}
			g := NewRemoteGateway(e.st, func() *remotegw.Client { return api })
			n, on, err := g.ServerVoiceName(context.Background(), srv.UUID)
			if err != nil || !on || n.OurHost != c.want.OurHost || n.PublicHost != c.want.PublicHost || n.PublicState != c.want.PublicState {
				t.Fatalf("got %+v %v %v, want %+v", n, on, err, c.want)
			}
			if got := "-"; n.NameTarget != nil {
				got = *n.NameTarget
				if got != c.target {
					t.Fatalf("name target = %q, want %q", got, c.target)
				}
			} else if c.target != "-" {
				t.Fatalf("name target = nil, want %q", c.target)
			}
			if want := c.state == "fallen_back"; (n.PublicStateAt != nil) != want || n.FailingSince != nil {
				t.Fatalf("state time %v, failing %v; want a state time: %v", n.PublicStateAt, n.FailingSince, want)
			}
		})
	}
}

// oldVoiceProvider is a proxy to base that answers like a provider older than
// the white-label name: no mod_host, public_host or public_state anywhere.
func oldVoiceProvider(t *testing.T, base string) string {
	t.Helper()
	var strip func(v interface{})
	strip = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			delete(x, "mod_host")
			delete(x, "public_host")
			delete(x, "public_state")
			for _, c := range x {
				strip(c)
			}
		case []interface{}:
			for _, c := range x {
				strip(c)
			}
		}
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest(r.Method, base+r.URL.Path, r.Body)
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var m interface{}
		if json.Unmarshal(raw, &m) == nil {
			strip(m)
			raw, _ = json.Marshal(m)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL
}

// Final review M2: the fall back's node write takes from the sweep budget
// before it starts, so a spent budget writes nothing and keeps the column for
// the next sweep.
func TestRemoteFallBackWriteTakesTheBudget(t *testing.T) {
	for _, c := range []struct {
		name               string
		budget, wantBudget int
		wantWrites         int
	}{
		{"budget spent", 0, 0, 0},
		{"budget left", 1, 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newExtrasEnv(t)
			rw := &rewrites{}
			_, d := e.addServer(1)
			name := "voice." + d
			e.fake.SetVoiceHost(d, ownVoiceName)
			port := e.voiceOn(t, d)
			e.setVoiceHost(d, ownVoiceName)
			e.setVoicePublicHost(d, name)
			e.later(outboxSweepEvery)
			srv := *e.st.servers[101]
			v := remotegw.RouteVoice{Enabled: true, Port: port, TargetPort: VoiceInternalPort, Host: ownVoiceName, ModHost: ownVoiceName, PublicHost: name, PublicState: "fallen_back"}
			p := remotegw.Route{Domain: d, LinkID: e.kit.LinkID, Voice: &v}
			budget := c.budget
			if err := reconcileRouteExtras(context.Background(), e.st, e.c, e.publish, rw.write, e.row(d), p, srv, &budget); err != nil {
				t.Fatal(err)
			}
			if got := rw.all(); len(got) != c.wantWrites || budget != c.wantBudget {
				t.Fatalf("rewrites %v budget %d, want %d writes budget %d", got, budget, c.wantWrites, c.wantBudget)
			}
			wantCol := name
			if c.wantWrites == 1 {
				wantCol = ""
			}
			if got := e.row(d).VoicePublicHost; got != wantCol {
				t.Fatalf("column %q, want %q", got, wantCol)
			}
		})
	}
}
