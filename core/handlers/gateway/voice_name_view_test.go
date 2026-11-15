package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// The hosted view carries what the panel's white-label states need, from
// Redis, settings and the stored checks only: nameTarget "" for a server with
// no own domain, our regional voice name when it has one; the failing stamp
// of an ACTIVE name whatever its status (a wrong-region miss is stored as ok);
// and the time of the last state change.
func TestVoiceViewNameFields(t *testing.T) {
	stamp := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	changed := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name        string
		ownDomain   bool
		state       string
		check       *store.AddressDNSCheck
		wantTarget  string
		wantFailing bool
	}{
		{"regional only: no name possible", false, "", nil, "", false},
		{"own domain: our regional voice name", true, "", nil, ourVoice, false},
		{"active, failing with status ok", true, "active", &store.AddressDNSCheck{Status: "ok", Region: "us", FailingSince: &stamp}, ourVoice, true},
		{"active, healthy", true, "active", &store.AddressDNSCheck{Status: "ok", Region: "eu"}, ourVoice, false},
		{"pending with a stamp: not failing", true, "pending", &store.AddressDNSCheck{Status: "not_found", FailingSince: &stamp}, ourVoice, false},
		{"fallen back with a stamp: not failing", true, "fallen_back", &store.AddressDNSCheck{Status: "not_found", FailingSince: &stamp}, ourVoice, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newVoiceSwitch(t)
			if c.ownDomain {
				e.addHostedRoute(odDomain)
			}
			e.fs.row = &store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Mod: "svc", Host: "a.eu.dylaris.com"}
			if c.state != "" {
				e.fs.row.PublicHost, e.fs.row.PublicState, e.fs.row.PublicStateAt = wlName, c.state, &changed
			}
			if c.check != nil {
				e.fs.dns = map[string]store.AddressDNSCheck{wlName: *c.check}
			}
			r := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/api/servers/7/voice", nil), map[string]string{"id": "7"})
			rec := httptest.NewRecorder()
			e.h.GetServerVoice(rec, r)
			var out struct {
				Voice voiceView `json:"voice"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			v := out.Voice
			if v.NameTarget == nil || *v.NameTarget != c.wantTarget {
				t.Fatalf("nameTarget = %v, want %q: %s", v.NameTarget, c.wantTarget, rec.Body.String())
			}
			if got := v.NameFailingSince != nil && v.NameFailingSince.Equal(stamp); got != c.wantFailing || (!c.wantFailing && v.NameFailingSince != nil) {
				t.Fatalf("nameFailingSince = %v, want failing %v", v.NameFailingSince, c.wantFailing)
			}
			if wantAt := c.state != ""; (v.PublicStateAt != nil && v.PublicStateAt.Equal(changed)) != wantAt {
				t.Fatalf("publicStateAt = %v, want set %v", v.PublicStateAt, wantAt)
			}
		})
	}
}

// Off carries none of the fields: there is nothing to offer.
func TestVoiceViewNameFieldsOff(t *testing.T) {
	e := newVoiceSwitch(t)
	e.addHostedRoute(odDomain)
	r := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/api/servers/7/voice", nil), map[string]string{"id": "7"})
	rec := httptest.NewRecorder()
	e.h.GetServerVoice(rec, r)
	if got, want := rec.Body.String(), `{"success":true,"voice":{"enabled":false,"host":"","port":0,"mod":"","allowed":true}}`+"\n"; got != want {
		t.Fatalf("off view %s, want %s", got, want)
	}
}

// The route-only view: name_target "" for an address on our domains (hoster
// or edge wildcard), our regional voice name for an own domain (the row's
// host when no edge of the region is online), the failing stamp only for an
// active name, and the state time from the row.
func TestLinkRouteVoiceViewNameFields(t *testing.T) {
	stamp := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	edges := []services.GatewayEdgeInfo{{Status: "online", Region: "eu", Wildcard: "*.eu.dylaris.com"}}
	ni := voiceNameInfo{
		reserved: voiceNameReserved([]services.HosterDomain{{Domain: "hoster.example"}}, edges),
		edges:    edges,
		checks:   map[string]store.AddressDNSCheck{wlName: {Status: "ok", Region: "us", FailingSince: &stamp}},
	}
	for _, c := range []struct {
		name        string
		row         store.LinkRouteVoice
		wantTarget  string
		wantFailing bool
	}{
		{"edge wildcard address", store.LinkRouteVoice{Domain: "r.eu.dylaris.com", Region: "eu"}, "", false},
		{"hoster domain address", store.LinkRouteVoice{Domain: "mc.hoster.example", Region: "eu"}, "", false},
		{"own domain", store.LinkRouteVoice{Domain: odDomain, Region: "eu", Host: ourVoice}, ourVoice, false},
		{"own domain, no online edge in the region", store.LinkRouteVoice{Domain: odDomain, Region: "us", Host: "voice.us.dylaris.com"}, "voice.us.dylaris.com", false},
		{"active and failing", store.LinkRouteVoice{Domain: odDomain, Region: "eu", Host: ourVoice, PublicHost: wlName, PublicState: "active", PublicStateAt: &stamp}, ourVoice, true},
		{"pending: no failing stamp", store.LinkRouteVoice{Domain: odDomain, Region: "eu", Host: ourVoice, PublicHost: wlName, PublicState: "pending", PublicStateAt: &stamp}, ourVoice, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newLinkRouteVoiceView(&c.row, ni)
			if v.NameTarget != c.wantTarget {
				t.Fatalf("name_target = %q, want %q", v.NameTarget, c.wantTarget)
			}
			if (v.FailingSince != nil) != c.wantFailing {
				t.Fatalf("failing_since = %v, want %v", v.FailingSince, c.wantFailing)
			}
			if v.PublicStateAt != c.row.PublicStateAt {
				t.Fatalf("public_state_at = %v, want the row's %v", v.PublicStateAt, c.row.PublicStateAt)
			}
		})
	}
	if got := activeVoiceNames(&store.LinkRouteVoice{PublicHost: "a", PublicState: "active"}, &store.LinkRouteVoice{PublicHost: "b", PublicState: "pending"}, nil); len(got) != 1 || got[0] != "a" {
		t.Fatalf("activeVoiceNames = %v", got)
	}
}

// On the managed gateway the provider's three fields pass through, the target
// also without a name (it decides whether one is offered); an older provider
// sends none and the view stays as it was.
func TestWithRemoteVoiceNamePassesTheFieldsThrough(t *testing.T) {
	stamp := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	empty, target := "", ourVoice
	base := voiceView{Enabled: true, Host: ourVoice, Port: 26000, Mod: "svc", Allowed: true}
	if v := withRemoteVoiceName(base, services.RemoteVoiceName{OurHost: ourVoice, NameTarget: &empty}); v.NameTarget == nil || *v.NameTarget != "" || v.PublicState != "" {
		t.Fatalf("no name, regional: %+v", v)
	}
	v := withRemoteVoiceName(base, services.RemoteVoiceName{OurHost: ourVoice, PublicHost: wlName, PublicState: "active", NameTarget: &target, FailingSince: &stamp, PublicStateAt: &stamp})
	if v.NameTarget == nil || *v.NameTarget != ourVoice || v.NameFailingSince != &stamp || v.PublicStateAt != &stamp || v.PublicState != "active" {
		t.Fatalf("active failing: %+v", v)
	}
	if v := withRemoteVoiceName(base, services.RemoteVoiceName{OurHost: ourVoice}); v != base {
		t.Fatalf("older provider: %+v, want %+v", v, base)
	}
}
