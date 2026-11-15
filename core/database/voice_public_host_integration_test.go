package database

import (
	"bytes"
	"testing"
	"time"

	"dylaris-core/store"
)

// Phase 71 on a database that predates it: old voice rows read no white-label
// name, a second boot is a no-op, and the CHECK refuses a state that is not
// one of the four.
func TestIntegrationVoicePublicHostSchema(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)
	rt := linkRoute(t, st)
	d := extrasDomains(t, st, 1)[0]
	region := voiceRegion()

	for _, q := range []string{
		`ALTER TABLE voice_ports DROP COLUMN IF EXISTS public_host`,
		`ALTER TABLE voice_ports DROP COLUMN IF EXISTS public_state`,
		`ALTER TABLE link_route_voice DROP COLUMN IF EXISTS public_host`,
		`ALTER TABLE link_route_voice DROP COLUMN IF EXISTS public_state`,
		`ALTER TABLE remote_routes DROP COLUMN IF EXISTS voice_public_host`,
		`ALTER TABLE voice_ports DROP COLUMN IF EXISTS public_state_at`,
		`ALTER TABLE link_route_voice DROP COLUMN IF EXISTS public_state_at`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO voice_ports (server_id, region, port, mod, internal_port, uplink_key, host) VALUES ($1, $2, 26700, 'svc', 24454, $3, 'play.example.test')`,
		f.server.ID, region, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO link_route_voice (domain, region, port, target_port, uplink_key) VALUES ($1, $2, 26701, 24454, $3)`,
		rt.Domain, region, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO remote_routes (domain, server_uuid, node_id, port, voice_port, voice_mod, voice_host) VALUES ($1, 'srv-old-ph', 1, 25565, 24454, 'svc', 'voice.eu.example.test')`, d); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := applyVoicePublicHost(db); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	vp, err := st.GetVoicePort(f.server.ID)
	if err != nil || vp == nil || vp.PublicHost != "" || vp.PublicState != "" || vp.PublicStateAt != nil || vp.Port != 26700 || vp.ModHost() != "play.example.test" {
		t.Fatalf("old voice row = %+v, %v; want no name and the mod host it had", vp, err)
	}
	v, err := st.GetLinkRouteVoice(rt.Domain)
	if err != nil || v == nil || v.PublicHost != "" || v.PublicState != "" || v.PublicStateAt != nil || v.ModHost() != rt.Domain {
		t.Fatalf("old route voice row = %+v, %v; want no name and the address as mod host", v, err)
	}
	r, err := st.GetRemoteRoute(d)
	if err != nil || r == nil || r.VoicePublicHost != "" || r.VoiceHost != "voice.eu.example.test" {
		t.Fatalf("old remote route = %+v, %v", r, err)
	}
	if _, err := db.Exec(`UPDATE voice_ports SET public_state = 'verified' WHERE server_id = $1`, f.server.ID); err == nil {
		t.Fatal("voice_ports: the CHECK let an unknown state in")
	}
	if _, err := db.Exec(`UPDATE link_route_voice SET public_state = 'verified' WHERE domain = $1`, rt.Domain); err == nil {
		t.Fatal("link_route_voice: the CHECK let an unknown state in")
	}
}

// The fall back moves only an active row that still holds the name it was
// asked about: a pending row, an already fallen back one and a name the
// customer changed meanwhile are left alone.
func TestIntegrationFallBackIsConditional(t *testing.T) {
	_, st := integrationDB(t)
	region := voiceRegion()
	f := newFixture(t, st)
	allocVoice(t, st, f.server.ID, region, 26000, 26999)
	rt := linkRoute(t, st)
	if _, err := st.AllocateLinkRouteVoice(rt.Domain, region, "", 24454, make([]byte, 32), 26000, 26999); err != nil {
		t.Fatal(err)
	}

	type side struct {
		name  string
		set   func(host, state string) error
		fall  func(host string) (bool, error)
		state func() (string, string)
	}
	sides := []side{
		{"server",
			func(h, s string) error { return st.SetVoicePublicHost(f.server.ID, h, s) },
			func(h string) (bool, error) { return st.FallBackVoicePublicHost(f.server.ID, h) },
			func() (string, string) {
				vp, err := st.GetVoicePort(f.server.ID)
				if err != nil || vp == nil {
					t.Fatalf("read = %+v, %v", vp, err)
				}
				return vp.PublicHost, vp.PublicState
			}},
		{"route",
			func(h, s string) error { return st.SetLinkRouteVoicePublicHost(rt.Domain, h, s) },
			func(h string) (bool, error) { return st.FallBackLinkRouteVoicePublicHost(rt.Domain, h) },
			func() (string, string) {
				v, err := st.GetLinkRouteVoice(rt.Domain)
				if err != nil || v == nil {
					t.Fatalf("read = %+v, %v", v, err)
				}
				return v.PublicHost, v.PublicState
			}},
	}
	for _, sd := range sides {
		t.Run(sd.name, func(t *testing.T) {
			for _, c := range []struct {
				name                string
				host, state, askFor string
				changed             bool
				wantHost, wantState string
			}{
				{"pending stays", "voice.a.example.test", "pending", "voice.a.example.test", false, "voice.a.example.test", "pending"},
				{"changed name stays", "voice.b.example.test", "active", "voice.a.example.test", false, "voice.b.example.test", "active"},
				{"fallen back stays", "voice.a.example.test", "fallen_back", "voice.a.example.test", false, "voice.a.example.test", "fallen_back"},
				{"active falls back", "voice.a.example.test", "active", "voice.a.example.test", true, "voice.a.example.test", "fallen_back"},
			} {
				if err := sd.set(c.host, c.state); err != nil {
					t.Fatal(err)
				}
				changed, err := sd.fall(c.askFor)
				if err != nil || changed != c.changed {
					t.Fatalf("%s: changed = %v, %v; want %v", c.name, changed, err, c.changed)
				}
				if h, s := sd.state(); h != c.wantHost || s != c.wantState {
					t.Fatalf("%s: row = %q %q, want %q %q", c.name, h, s, c.wantHost, c.wantState)
				}
			}
			if err := sd.set("", ""); err != nil {
				t.Fatal(err)
			}
			if h, s := sd.state(); h != "" || s != "" {
				t.Fatalf("cleared row = %q %q", h, s)
			}
		})
	}
}

// Turning voice on again in the same region keeps the name and its state;
// off deletes the row and so the name; a fresh row starts without one. On a
// remote route the name goes with the voice port.
func TestIntegrationReenableKeepsPublicHost(t *testing.T) {
	_, st := integrationDB(t)
	region := voiceRegion()
	f := newFixture(t, st)
	first := allocVoice(t, st, f.server.ID, region, 26000, 26999)
	if err := st.SetVoicePublicHost(f.server.ID, "voice.play.example.test", store.VoicePublicActive); err != nil {
		t.Fatal(err)
	}
	again := allocVoice(t, st, f.server.ID, region, 26000, 26999)
	if again.Port != first.Port || again.PublicHost != "voice.play.example.test" || again.PublicState != "active" || again.ModHost() != "voice.play.example.test" {
		t.Fatalf("re-enable = %+v; want the port and the active name kept", again)
	}
	if ok, err := st.UpdateVoicePortMod(f.server.ID, region, again.Port, "plasmo", 24454, again.Host); err != nil || !ok {
		t.Fatalf("mod update = %v, %v", ok, err)
	}
	if vp, _ := st.GetVoicePort(f.server.ID); vp == nil || vp.PublicState != "active" {
		t.Fatalf("after a mod update = %+v; want the name kept", vp)
	}
	if err := st.DeleteVoicePort(f.server.ID); err != nil {
		t.Fatal(err)
	}
	if fresh := allocVoice(t, st, f.server.ID, region, 26000, 26999); fresh.PublicHost != "" || fresh.PublicState != "" {
		t.Fatalf("after off and on = %+v; want no name", fresh)
	}

	rt := linkRoute(t, st)
	key := bytes.Repeat([]byte{0x42}, 32)
	if _, err := st.AllocateLinkRouteVoice(rt.Domain, region, "voice.eu.example.test", 24454, key, 26000, 26999); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLinkRouteVoicePublicHost(rt.Domain, "voice.route.example.test", store.VoicePublicPending); err != nil {
		t.Fatal(err)
	}
	v, err := st.AllocateLinkRouteVoice(rt.Domain, region, "voice.eu.example.test", 24455, key, 26000, 26999)
	if err != nil || v.PublicHost != "voice.route.example.test" || v.PublicState != "pending" || v.ModHost() != "voice.eu.example.test" {
		t.Fatalf("route re-enable = %+v, %v; want the pending name kept and our name for the mod", v, err)
	}
	if _, err := st.DeleteLinkRouteVoice(rt.Domain); err != nil {
		t.Fatal(err)
	}
	if v, err := st.AllocateLinkRouteVoice(rt.Domain, region, "", 24454, key, 26000, 26999); err != nil || v.PublicHost != "" || v.PublicState != "" {
		t.Fatalf("route after off and on = %+v, %v; want no name", v, err)
	}

	d := extrasDomains(t, st, 1)[0]
	if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: d, ServerUUID: "srv-ph", NodeID: 1, Port: 25565}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRemoteRouteVoice(d, intp(24490), "svc", "voice.eu.example.test"); err != nil {
		t.Fatal(err)
	}
	if found, err := st.SetRemoteRouteVoicePublicHost(d, "voice.remote.example.test"); err != nil || !found {
		t.Fatalf("set remote name = %v, %v", found, err)
	}
	if _, err := st.SetRemoteRouteVoice(d, intp(24490), "svc", "voice.eu.example.test"); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.GetRemoteRoute(d); r == nil || r.VoicePublicHost != "voice.remote.example.test" {
		t.Fatalf("remote re-assert = %+v; want the name kept", r)
	}
	if _, err := st.SetRemoteRouteVoice(d, nil, "", ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.GetRemoteRoute(d); r == nil || r.VoicePublicHost != "" {
		t.Fatalf("remote off = %+v; want the name gone", r)
	}
}

// public_state_at moves when the state changes, by a set or a fall back, and
// only then: the same state again (a Check again that stays pending) keeps
// the stamp, and clearing the name clears it.
func TestIntegrationPublicStateAt(t *testing.T) {
	db, st := integrationDB(t)
	region := voiceRegion()
	f := newFixture(t, st)
	allocVoice(t, st, f.server.ID, region, 26000, 26999)
	rt := linkRoute(t, st)
	if _, err := st.AllocateLinkRouteVoice(rt.Domain, region, "", 24454, make([]byte, 32), 26000, 26999); err != nil {
		t.Fatal(err)
	}
	type side struct {
		name, table, key string
		set              func(host, state string) error
		fall             func(host string) (bool, error)
		at               func() *time.Time
	}
	sides := []side{
		{"server", "voice_ports", "server_id",
			func(h, s string) error { return st.SetVoicePublicHost(f.server.ID, h, s) },
			func(h string) (bool, error) { return st.FallBackVoicePublicHost(f.server.ID, h) },
			func() *time.Time {
				vp, err := st.GetVoicePort(f.server.ID)
				if err != nil || vp == nil {
					t.Fatalf("read = %+v, %v", vp, err)
				}
				return vp.PublicStateAt
			}},
		{"route", "link_route_voice", "domain",
			func(h, s string) error { return st.SetLinkRouteVoicePublicHost(rt.Domain, h, s) },
			func(h string) (bool, error) { return st.FallBackLinkRouteVoicePublicHost(rt.Domain, h) },
			func() *time.Time {
				v, err := st.GetLinkRouteVoice(rt.Domain)
				if err != nil || v == nil {
					t.Fatalf("read = %+v, %v", v, err)
				}
				return v.PublicStateAt
			}},
	}
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, sd := range sides {
		t.Run(sd.name, func(t *testing.T) {
			var keyArg interface{} = f.server.ID
			if sd.key == "domain" {
				keyArg = rt.Domain
			}
			// Backdate the stamp so a NOW() write is told apart from a kept one.
			backdate := func() {
				t.Helper()
				if _, err := db.Exec(`UPDATE `+sd.table+` SET public_state_at = $2 WHERE `+sd.key+` = $1`, keyArg, old); err != nil {
					t.Fatal(err)
				}
			}
			moved := func(what string, want bool) {
				t.Helper()
				at := sd.at()
				if at == nil {
					t.Fatalf("%s: stamp is nil", what)
				}
				if got := !at.Equal(old); got != want {
					t.Fatalf("%s: stamp %v, moved = %v, want %v", what, at, got, want)
				}
			}
			if at := sd.at(); at != nil {
				t.Fatalf("no name yet: stamp = %v", at)
			}
			if err := sd.set("voice.a.example.test", "pending"); err != nil {
				t.Fatal(err)
			}
			if sd.at() == nil {
				t.Fatal("pending: no stamp")
			}
			backdate()
			if err := sd.set("voice.a.example.test", "pending"); err != nil {
				t.Fatal(err)
			}
			moved("pending again", false)
			if err := sd.set("voice.a.example.test", "active"); err != nil {
				t.Fatal(err)
			}
			moved("active", true)
			backdate()
			if changed, err := sd.fall("voice.a.example.test"); err != nil || !changed {
				t.Fatalf("fall back = %v, %v", changed, err)
			}
			moved("fallen back", true)
			backdate()
			if changed, err := sd.fall("voice.a.example.test"); err != nil || changed {
				t.Fatalf("second fall back = %v, %v", changed, err)
			}
			moved("second fall back", false)
			if err := sd.set("", ""); err != nil {
				t.Fatal(err)
			}
			if at := sd.at(); at != nil {
				t.Fatalf("cleared: stamp = %v", at)
			}
		})
	}
}
