package gateway

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
	"dylaris-core/services/voicecfg"
)

// --- Managed gateway (remote mode): the white-label voice name. Only the
// provider resolves; this Core passes its answers through. ---

const remoteName = "voice." + remoteOwn

// newRemoteVoiceName is a server on the managed gateway whose own-domain
// address has voice on, the mod told the provider's regional name.
func newRemoteVoiceName(t *testing.T) (*remoteExtrasEnv, *odResolver) {
	t.Helper()
	e, res := newRemoteOwnDomain(t)
	e.fake.SetVoiceHost(remoteOwn, remoteOwnVoice)
	if rec, _ := e.put(t, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("voice on: %d %s", rec.Code, rec.Body.String())
	}
	return e, res
}

func TestRemoteVoiceNameActiveWritesConfig(t *testing.T) {
	e, res := newRemoteVoiceName(t)
	before := handlertest.VoiceFixture(t, "svc_before.properties")

	n := len(e.written())
	rec, out := e.putName(t, remoteName)
	if rec.Code != http.StatusOK || out["restartRequired"] != true {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	if v, _ := out["voice"].(map[string]interface{}); v["host"] != remoteName || v["port"] != float64(30000) || v["mod"] != "svc" {
		t.Fatalf("voice %v", v)
	}
	if got, want := e.written()[n:], voicecfg.SvcEnable(before, remoteName, 30000); got != want {
		t.Fatalf("config written:\n%s\nwant:\n%s", got, want)
	}
	if row := e.rs.row(remoteOwn); row.VoicePublicHost != remoteName || row.VoiceHost != remoteOwnVoice || row.VoicePort == nil || *row.VoicePort != 30000 {
		t.Fatalf("column %+v", row)
	}
	if _, view := e.getVoice(t); view.Host != remoteName || view.Port != 30000 {
		t.Fatalf("view %+v", view)
	}

	// Check again of the same active name: written again (idempotent), no
	// restart needed.
	if rec, out := e.putName(t, remoteName); rec.Code != http.StatusOK || out["restartRequired"] != false {
		t.Fatalf("check again: %d %s", rec.Code, rec.Body.String())
	}

	// Another name the provider does not activate: its 409, and the active
	// name stays here as it stays there.
	e.fake.SetVoiceNameState(remoteOwn, "pending")
	n = len(e.written())
	if rec, out := e.putName(t, "v2."+remoteOwn); rec.Code != http.StatusConflict || out["code"] != "voice_host_not_pointing" {
		t.Fatalf("second name: %d %s", rec.Code, rec.Body.String())
	}
	if row := e.rs.row(remoteOwn); row.VoicePublicHost != remoteName || len(e.written()) != n {
		t.Fatalf("a refusal changed the column %+v or the config", row)
	}

	// Cancel: the provider's own host back into the mod, the column cleared.
	n = len(e.written())
	rec, out = e.putName(t, "")
	if rec.Code != http.StatusOK || out["restartRequired"] != true {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	if got, want := e.written()[n:], voicecfg.SvcEnable(before, remoteOwnVoice, 30000); got != want {
		t.Fatalf("config after cancel:\n%s\nwant:\n%s", got, want)
	}
	if row := e.rs.row(remoteOwn); row.VoicePublicHost != "" {
		t.Fatalf("column after cancel %+v", row)
	}
	if res.count() != 0 {
		t.Fatalf("the customer Core made %d DNS lookups, want 0", res.count())
	}
}

// Each provider refusal is passed through with its code and its own sentence,
// and nothing is written here: no column, no config, no lookup.
func TestRemoteVoiceNameRefusalsPassThrough(t *testing.T) {
	for _, c := range []struct {
		name, state, set, code string
		status                 int
	}{
		{"not the address or under it", "", "voice.other.tld", "voice_host_invalid", http.StatusBadRequest},
		{"does not point at the region", "pending", remoteName, "voice_host_not_pointing", http.StatusConflict},
		{"provider could not look it up", "dns_lookup_failed", remoteName, "dns_lookup_failed", http.StatusServiceUnavailable},
		{"provider's DNS budget", "rate_limited", remoteName, "rate_limited", http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, res := newRemoteVoiceName(t)
			if c.state != "" {
				e.fake.SetVoiceNameState(remoteOwn, c.state)
			}
			n := len(e.written())
			rec, out := e.putName(t, c.set)
			if rec.Code != c.status || out["code"] != c.code || out["error"] != remotegwtest.VoiceNameMessage(c.code, c.set) {
				t.Fatalf("%d %s, want %d %s with the provider's sentence", rec.Code, rec.Body.String(), c.status, c.code)
			}
			if row := e.rs.row(remoteOwn); row.VoicePublicHost != "" || len(e.written()) != n || res.count() != 0 {
				t.Fatalf("column %+v, config written %v, lookups %d", row, len(e.written()) != n, res.count())
			}
		})
	}
	t.Run("voice off", func(t *testing.T) {
		e, _ := newRemoteOwnDomain(t)
		calls := len(e.fake.Calls())
		if rec, out := e.putName(t, remoteName); rec.Code != http.StatusConflict || out["code"] != "voice_off" || len(e.fake.Calls()) != calls {
			t.Fatalf("%d %s, provider calls %d", rec.Code, rec.Body.String(), len(e.fake.Calls())-calls)
		}
	})
	t.Run("node write fails", func(t *testing.T) {
		e, _ := newRemoteVoiceName(t)
		e.node.FailWrite = true
		if rec, _ := e.putName(t, remoteName); rec.Code != http.StatusBadGateway {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		// The provider holds the name; the column waits for a retry that
		// writes the config.
		if row := e.rs.row(remoteOwn); row.VoicePublicHost != "" {
			t.Fatalf("column %+v", row)
		}
		e.node.FailWrite = false
		if rec, out := e.putName(t, remoteName); rec.Code != http.StatusOK || out["restartRequired"] != true || e.rs.row(remoteOwn).VoicePublicHost != remoteName {
			t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
		}
	})
}

// Review Focus 7: a provider older than the white-label name ignores
// public_host and answers without mod_host (7b-2 era: without host too). The
// set is refused in this Core's words and nothing is written.
func TestRemoteVoiceNameOnAnOldProvider(t *testing.T) {
	for _, c := range []struct {
		name string
		keys []string
	}{
		{"part 1 provider", []string{"mod_host", "public_host", "public_state"}},
		{"7b-2 provider", []string{"host", "mod_host", "public_host", "public_state"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _ := newRemoteVoiceName(t)
			e.base = stripVoiceKeys(t, e.fake.URL(), c.keys...)
			n := len(e.written())
			rec, out := e.putName(t, remoteName)
			if rec.Code != http.StatusConflict || out["code"] != "voice_name_unavailable" {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if row := e.rs.row(remoteOwn); row.VoicePublicHost != "" || len(e.written()) != n {
				t.Fatalf("column %+v, config written", row)
			}
		})
	}
}

// A provider answer is written into a mod config file only when it is the
// name asked for (a set) or the host stored here (a cancel), and a plain DNS
// name: a newline would add lines to voicechat-server.properties.
func TestRemoteVoiceNameInvalidModHost(t *testing.T) {
	for _, c := range []struct{ name, set, mod string }{
		{"not a plain DNS name", remoteName, "evil.tld\nvoice_port=1"},
		{"another name than the one set", remoteName, "voice.attacker.tld"},
		{"a cancel naming a host of its own", "", "voice.attacker.tld"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _ := newRemoteVoiceName(t)
			e.base = rewriteVoiceKey(t, e.fake.URL(), "mod_host", c.mod)
			n := len(e.written())
			if rec, out := e.putName(t, c.set); rec.Code != http.StatusBadGateway || out["code"] != "provider_error" {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if row := e.rs.row(remoteOwn); row.VoicePublicHost != "" || len(e.written()) != n {
				t.Fatalf("column %+v, config written", row)
			}
		})
	}
}

// The white-label state in the view (Task 18): publicHost and publicState
// from the provider (its answer on a set, its cached route list on a GET),
// ourHost and host from this Core's columns. Without the list, or from an
// older provider, the column alone: a name here is active.
func TestRemoteVoiceViewCarriesTheNameState(t *testing.T) {
	e, _ := newRemoteVoiceName(t)
	e.fake.SetVoiceNameState(remoteOwn, "pending")
	if rec, _ := e.putName(t, remoteName); rec.Code != http.StatusConflict {
		t.Fatalf("pending: %d %s", rec.Code, rec.Body.String())
	}
	if _, v := e.getVoice(t); v.Host != remoteOwnVoice || v.PublicHost != remoteName || v.PublicState != "pending" || v.OurHost != remoteOwnVoice || v.ModHost != remoteOwnVoice {
		t.Fatalf("pending view %+v", v)
	}

	e.fake.SetVoiceNameState(remoteOwn, "active")
	rec, out := e.putName(t, remoteName)
	if v, _ := out["voice"].(map[string]interface{}); rec.Code != http.StatusOK || v["publicHost"] != remoteName || v["publicState"] != "active" || v["ourHost"] != remoteOwnVoice || v["modHost"] != remoteName {
		t.Fatalf("set answer %d %s", rec.Code, rec.Body.String())
	}
	if _, v := e.getVoice(t); v.Host != remoteName || v.PublicState != "active" || v.OurHost != remoteOwnVoice {
		t.Fatalf("active view %+v", v)
	}

	for _, c := range []struct{ name, base string }{
		{"list unreadable", "http://127.0.0.1:1"},
		{"older provider", stripVoiceKeys(t, e.fake.URL(), "mod_host", "public_host", "public_state")},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.base = c.base
			e.h.d.RemoteGateway = newRemoteGatewayAt(e)
			rec, v := e.getVoice(t)
			if rec.Code != http.StatusOK || v.Host != remoteName || v.PublicHost != remoteName || v.PublicState != "active" || v.OurHost != remoteOwnVoice {
				t.Fatalf("%d view %+v", rec.Code, v)
			}
		})
	}

	// Cancel: no name, so the view is the one without white-label fields.
	e.base = e.fake.URL()
	e.h.d.RemoteGateway = newRemoteGatewayAt(e)
	if rec, out := e.putName(t, ""); rec.Code != http.StatusOK {
		t.Fatalf("cancel %d %v", rec.Code, out)
	} else if v, _ := out["voice"].(map[string]interface{}); v["publicState"] != nil || v["ourHost"] != nil {
		t.Fatalf("cancel answer %v", v)
	}
	if _, v := e.getVoice(t); v.Host != remoteOwnVoice || v.PublicState != "" || v.OurHost != "" {
		t.Fatalf("cancelled view %+v", v)
	}
}

// newRemoteGatewayAt is a fresh RemoteGateway on e.base: no cached route list.
func newRemoteGatewayAt(e *remoteExtrasEnv) *services.RemoteGateway {
	return services.NewRemoteGateway(e.rs, func() *remotegw.Client { return remotegw.New(e.base, e.fake.Key) })
}

// The sweep's writer: it writes only while the columns still name from on
// port, and says so, so the sweep clears its column only after a write that
// landed.
func TestRewriteRemoteVoiceHost(t *testing.T) {
	e, _ := newRemoteVoiceName(t)
	if rec, _ := e.putName(t, remoteName); rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	srv := e.fs.server
	n := len(e.written())
	if err := RewriteRemoteVoiceHost(context.Background(), e.h.d.RemoteGateway, e.h.d.GRPCRegistry, &srv, "v2."+remoteOwn, remoteOwnVoice, 30000); !errors.Is(err, services.ErrVoiceRowChanged) || len(e.written()) != n {
		t.Fatalf("another name in the row: %v, written %v", err, len(e.written()) != n)
	}
	if err := RewriteRemoteVoiceHost(context.Background(), e.h.d.RemoteGateway, e.h.d.GRPCRegistry, &srv, remoteName, remoteOwnVoice, 30000); err != nil {
		t.Fatal(err)
	}
	if got, want := e.written()[n:], voicecfg.SvcEnable(handlertest.VoiceFixture(t, "svc_before.properties"), remoteOwnVoice, 30000); got != want {
		t.Fatalf("config written:\n%s\nwant:\n%s", got, want)
	}
	e.node.FailWrite = true
	if err := RewriteRemoteVoiceHost(context.Background(), e.h.d.RemoteGateway, e.h.d.GRPCRegistry, &srv, remoteName, remoteOwnVoice, 30000); err == nil {
		t.Fatal("a failed node write reported success")
	}
}
