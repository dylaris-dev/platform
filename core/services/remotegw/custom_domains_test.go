package remotegw_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"dylaris-core/services/remotegw"
)

// The provider's own-domain endpoints as the fake serves them, which follows
// the real handlers (the shapes are pinned against those in
// handlers/gateway/external_contract_test.go).
func TestClientCustomDomains(t *testing.T) {
	ctx := context.Background()

	t.Run("list unwraps items, add returns the provider TXT value", func(t *testing.T) {
		cl, _ := newClient(t)
		d, err := cl.AddCustomDomain(ctx, "play.own.net")
		if err != nil || d.Domain != "play.own.net" || d.State != "pending" ||
			d.TXTName != "_dylaris-verify.play.own.net" || !strings.HasPrefix(d.TXTValue, "dylaris-verify=") {
			t.Fatalf("add = %+v, %v", d, err)
		}
		again, err := cl.AddCustomDomain(ctx, "play.own.net")
		if err != nil || again.TXTValue != d.TXTValue {
			t.Fatalf("second add = %+v, %v; want the same TXT value", again, err)
		}
		items, err := cl.CustomDomains(ctx)
		if err != nil || len(items) != 1 || items[0] != d {
			t.Fatalf("list = %+v, %v", items, err)
		}
		issued, err := cl.IssueCustomDomainTXT(ctx, "play.own.net")
		if err != nil || issued.TXTValue != d.TXTValue {
			t.Fatalf("issue = %+v, %v", issued, err)
		}
	})

	t.Run("add refusals", func(t *testing.T) {
		cl, f := newClient(t)
		f.SetCustomDomainsEnabled(false)
		if _, err := cl.AddCustomDomain(ctx, "a.own.net"); !isAPI(err, 409, "custom_domains_off") {
			t.Fatalf("own domains off: %v", err)
		}
		f.SetCustomDomainsEnabled(true)
		for i := 0; i < 50; i++ {
			if _, err := cl.AddCustomDomain(ctx, "d"+strings.Repeat("x", i)+".own.net"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := cl.AddCustomDomain(ctx, "one-too-many.own.net"); !isAPI(err, 409, "too_many_domains") {
			t.Fatalf("past the claim bound: %v", err)
		}
		// A domain already held is not a new one.
		if _, err := cl.AddCustomDomain(ctx, "d.own.net"); err != nil {
			t.Fatalf("re-add at the bound: %v", err)
		}
	})

	for _, c := range []struct {
		name      string
		setup     func(cl *remotegw.Client, f fakeKnobs)
		wantOK    bool
		wantErr   bool
		wantCode  string
		wantState int
	}{
		{"verified (200)", func(cl *remotegw.Client, f fakeKnobs) {
			cl.AddCustomDomain(ctx, "v.own.net")
			f.SetTXTPublished("v.own.net", true)
		}, true, false, "", 0},
		{"not found yet (409 conflict) is false with no error", func(cl *remotegw.Client, f fakeKnobs) {
			cl.AddCustomDomain(ctx, "v.own.net")
		}, false, false, "", 0},
		{"cooldown (429)", func(cl *remotegw.Client, f fakeKnobs) {
			cl.AddCustomDomain(ctx, "v.own.net")
			cl.VerifyCustomDomainTXT(ctx, "v.own.net")
		}, false, true, "verify_cooldown", 429},
		{"unknown domain (404)", func(*remotegw.Client, fakeKnobs) {}, false, true, "not_found", 404},
		{"already verified (400)", func(cl *remotegw.Client, f fakeKnobs) {
			cl.AddCustomDomain(ctx, "v.own.net")
			f.SetCustomDomainState("v.own.net", "verified")
		}, false, true, "bad_request", 400},
	} {
		t.Run("verify "+c.name, func(t *testing.T) {
			cl, f := newClient(t)
			c.setup(cl, f)
			ok, err := cl.VerifyCustomDomainTXT(ctx, "v.own.net")
			if ok != c.wantOK || (err != nil) != c.wantErr {
				t.Fatalf("verify = %v, %v", ok, err)
			}
			if c.wantErr && !isAPI(err, c.wantState, c.wantCode) {
				t.Fatalf("err = %v, want %d %s", err, c.wantState, c.wantCode)
			}
		})
	}

	t.Run("delete: done, 404 is done, in use refused", func(t *testing.T) {
		cl, f := newClient(t)
		cl.AddCustomDomain(ctx, "gone.own.net")
		if err := cl.DeleteCustomDomain(ctx, "gone.own.net"); err != nil {
			t.Fatal(err)
		}
		if err := cl.DeleteCustomDomain(ctx, "gone.own.net"); err != nil {
			t.Fatalf("second delete (404) = %v, want done", err)
		}
		cl.AddCustomDomain(ctx, "used.own.net")
		f.AddRoute("link-x", "used.own.net", "mc-0b6f1c2e-6a3d-4c1b-9e7f-1a2b3c4d5e6f", 25565)
		if err := cl.DeleteCustomDomain(ctx, "used.own.net"); !isAPI(err, 409, "domain_in_use") {
			t.Fatalf("in use: %v", err)
		}
		if err := cl.DeleteCustomDomain(ctx, ".."); !errors.Is(err, remotegw.ErrBadSegment) {
			t.Fatalf("bad segment: %v", err)
		}
	})

	t.Run("the key is never in an error", func(t *testing.T) {
		cl, f := newClient(t)
		f.FailNext(1, http.StatusUnauthorized, "", 0)
		_, e1 := cl.CustomDomains(ctx)
		_, e2 := cl.VerifyCustomDomainTXT(ctx, "k.own.net")
		_, e3 := cl.IssueCustomDomainTXT(ctx, "k.own.net")
		f.SetCustomDomainsEnabled(false)
		_, e4 := cl.AddCustomDomain(ctx, "k.own.net")
		for _, err := range []error{e1, e2, e3, e4} {
			if err == nil || strings.Contains(err.Error(), f.Key()) {
				t.Fatalf("err = %v", err)
			}
		}
	})
}

// The fake's own-domain behaviour that later work builds on, as the real
// provider handlers do it.
func TestFakeOwnDomainsLikeTheProvider(t *testing.T) {
	ctx := context.Background()

	t.Run("add: trailing dot trimmed, hoster domains and bad names refused", func(t *testing.T) {
		cl, _ := newClient(t)
		if d, err := cl.AddCustomDomain(ctx, "Trail.own.net."); err != nil || d.Domain != "trail.own.net" {
			t.Fatalf("trailing dot: %+v, %v", d, err)
		}
		for _, bad := range []string{"x.example.com", "example.com", "bad_name.net", "single", "a.b.c.d.e.net"} {
			if _, err := cl.AddCustomDomain(ctx, bad); !isAPI(err, 400, "bad_request") {
				t.Errorf("%s: %v, want 400 bad_request", bad, err)
			}
		}
	})

	t.Run("route put on an own domain needs a verified claim", func(t *testing.T) {
		cl, f := newClient(t)
		kit, err := cl.MintKit(ctx, "k", "")
		if err != nil {
			t.Fatal(err)
		}
		put := func() error {
			return cl.PutRoute(ctx, kit.LinkID, "play.own.net", "mc-0b6f1c2e-6a3d-4c1b-9e7f-1a2b3c4d5e6f", 25565)
		}
		// No claim yet: the gate makes one and names its TXT value.
		err = put()
		items, _ := cl.CustomDomains(ctx)
		if !isAPI(err, 403, "domain_unverified") || len(items) != 1 || !strings.Contains(err.Error(), items[0].TXTValue) {
			t.Fatalf("no claim: %v, claims %+v", err, items)
		}
		f.SetCustomDomainState("play.own.net", "permablocked")
		if err := put(); !isAPI(err, 403, "domain_blocked") {
			t.Fatalf("permablocked: %v", err)
		}
		f.SetCustomDomainState("play.own.net", "blocked")
		f.SetTXTPublished("play.own.net", true)
		if err := put(); !isAPI(err, 403, "domain_unverified") {
			t.Fatalf("published, live re-proof off: %v", err)
		}
		f.SetLiveReverify(true)
		if err := put(); err != nil {
			t.Fatalf("live re-proof: %v", err)
		}
		if items, _ := cl.CustomDomains(ctx); items[0].State != "verified" {
			t.Fatalf("claim after the re-proof = %+v", items[0])
		}
		// A hoster domain is not gated.
		if err := cl.PutRoute(ctx, kit.LinkID, "a.example.com", "mc-0b6f1c2e-6a3d-4c1b-9e7f-1a2b3c4d5e6f", 25565); err != nil {
			t.Fatalf("hoster domain: %v", err)
		}
	})

	t.Run("a verified claim failing its re-checks is checked again", func(t *testing.T) {
		cl, f := newClient(t)
		cl.AddCustomDomain(ctx, "v.own.net")
		f.SetCustomDomainState("v.own.net", "verified")
		at := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
		f.SetCustomDomainLapsesAt("v.own.net", &at)
		items, _ := cl.CustomDomains(ctx)
		if items[0].LapsesAt == nil || !items[0].LapsesAt.Equal(at) {
			t.Fatalf("lapsesAt = %v, want %v", items[0].LapsesAt, at)
		}
		if ok, err := cl.VerifyCustomDomainTXT(ctx, "v.own.net"); ok || err != nil {
			t.Fatalf("record still missing: %v, %v; want not verified, no error", ok, err)
		}
		f2cl, f2 := newClient(t)
		f2cl.AddCustomDomain(ctx, "v.own.net")
		f2.SetCustomDomainState("v.own.net", "verified")
		f2.SetCustomDomainLapsesAt("v.own.net", &at)
		f2.SetTXTPublished("v.own.net", true)
		if ok, err := f2cl.VerifyCustomDomainTXT(ctx, "v.own.net"); !ok || err != nil {
			t.Fatalf("record back: %v, %v", ok, err)
		}
		if items, _ := f2cl.CustomDomains(ctx); items[0].LapsesAt != nil {
			t.Fatalf("still lapsing after the re-check: %+v", items[0])
		}
	})
}

type fakeKnobs interface {
	SetTXTPublished(domain string, ok bool)
	SetCustomDomainState(domain, state string)
}

func isAPI(err error, status int, code string) bool {
	var ae *remotegw.APIError
	return errors.As(err, &ae) && ae.Status == status && ae.Code == code
}
