package gateway

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// --- Bedrock on an own-domain address on the managed gateway (part 2). The
// provider checks the DNS; this Core passes its answers through and never
// resolves. Fixture in gateway_voice_remote_test.go. ---

func TestRemoteOwnDomainBedrockAdd(t *testing.T) {
	t.Run("the provider says yes", func(t *testing.T) {
		e, res := newRemoteOwnDomain(t)
		rec, out := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteOwn, true, true), false)
		if rec.Code != http.StatusOK || out["restartRequired"] != true {
			t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
		}
		want := providerKey(t, e.fake, remoteOwn)
		if string(e.rs.key) != string(want) {
			t.Fatalf("stored key %s, provider's %s", keyID(e.rs.key), keyID(want))
		}
		if e.publishedKey() != base64.StdEncoding.EncodeToString(want) {
			t.Fatalf("published key is not the provider's (want %s)", keyID(want))
		}
		if ed := e.rs.row(remoteOwn).Edition; ed != "both" {
			t.Fatalf("edition %q, want both", ed)
		}
		if n := res.count(); n != 0 {
			t.Fatalf("the customer Core made %d DNS lookups, want 0: only the provider resolves", n)
		}
	})

	// The provider's sentences are passed through; nothing is kept here.
	for _, c := range []struct {
		name, code, msg string
		status          int
	}{
		{"the provider's DNS sentence", "bedrock_unavailable",
			"play.acme.tld reaches the gateway only through an SRV record, and Bedrock ignores SRV records. Add a CNAME from play.acme.tld to the address shown under Your own domains, or give Bedrock to an address on one of the gateway's domains.",
			http.StatusConflict},
		{"the provider could not look it up", "dns_lookup_failed",
			"The lookup of play.acme.tld failed right now. Nothing was changed. Try again in a moment.", http.StatusServiceUnavailable},
		{"the provider's DNS budget", "rate_limited", "Too many DNS checks. Wait a minute.", http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, res := newRemoteOwnDomain(t)
			e.fake.SetBedrockDNS(remoteOwn, c.code, c.msg)
			rec, out := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteOwn, true, true), false)
			if rec.Code != c.status || out["code"] != c.code || out["error"] != c.msg {
				t.Fatalf("status %d body %s, want %d %s %q", rec.Code, rec.Body.String(), c.status, c.code, c.msg)
			}
			e.assertNoBedrock(t)
			if n := res.count(); n != 0 {
				t.Fatalf("%d DNS lookups, want 0", n)
			}
		})
	}
}

// Review Focus 7: a provider older than part 2 refuses an own domain with its
// old sentence. It is passed through (it names the provider's domains, which
// is right on the customer's side too) and the column goes back.
func TestRemoteOwnDomainBedrockOnAnOldProvider(t *testing.T) {
	const old = "Bedrock needs an address on one of the platform's domains."
	e, _ := newRemoteOwnDomain(t)
	e.fake.SetBedrockDNS(remoteOwn, "bedrock_unavailable", old)
	rec, out := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteOwn, true, true), false)
	if rec.Code != http.StatusConflict || out["code"] != "bedrock_unavailable" || out["error"] != old {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	e.assertNoBedrock(t)
	// The view of an older provider carries no DNS fields: none are shown.
	_, view := bedrockCall(e.voiceSwitchEnv, http.MethodGet, "", false)
	raw, _ := json.Marshal(view)
	var got struct {
		Bedrock bedrockView `json:"bedrock"`
	}
	_ = json.Unmarshal(raw, &got)
	if len(got.Bedrock.Routes) != 1 || got.Bedrock.Routes[0].DNSStatus != "" || got.Bedrock.Routes[0].DNSCheckedAt != nil {
		t.Fatalf("routes %+v", got.Bedrock.Routes)
	}
}

// The view carries the provider's last stored check of an own domain as it
// came, never a lookup of its own; an address on the provider's domains shows
// none.
func TestRemoteOwnDomainBedrockDNSStateInView(t *testing.T) {
	e, res := newRemoteOwnDomain(t)
	e.addAddress(remoteAddr, false)
	checked := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)
	since := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	e.fake.SetBedrockDNSState(remoteOwn, "elsewhere", &checked, &since)
	rec, out := bedrockCall(e.voiceSwitchEnv, http.MethodGet, "", false)
	raw, _ := json.Marshal(out)
	var got struct {
		Bedrock bedrockView `json:"bedrock"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	v := got.Bedrock
	// The regional address is the one asked about, as before part 2.
	if v.Address != remoteAddr || !v.Available || len(v.Routes) != 1 {
		t.Fatalf("view %+v", v)
	}
	r := v.Routes[0]
	if r.Domain != remoteOwn || !r.OwnDomain || r.Bedrock || r.DNSStatus != "elsewhere" ||
		r.DNSCheckedAt == nil || !r.DNSCheckedAt.Equal(checked) || r.DNSFailingSince == nil || !r.DNSFailingSince.Equal(since) {
		t.Fatalf("route %+v", r)
	}

	// Once it has Bedrock it is the address, and still shows its state.
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteOwn, true, true), false); rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	_, out = bedrockCall(e.voiceSwitchEnv, http.MethodGet, "", false)
	raw, _ = json.Marshal(out)
	got.Bedrock = bedrockView{}
	_ = json.Unmarshal(raw, &got)
	if v := got.Bedrock; v.Address != remoteOwn || !v.Enabled || len(v.Routes) != 1 || v.Routes[0].DNSStatus != "elsewhere" || !v.Routes[0].Bedrock {
		t.Fatalf("view after the add %+v", v)
	}
	if n := res.count(); n != 0 {
		t.Fatalf("%d DNS lookups, want 0", n)
	}
}
