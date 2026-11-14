package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"
)

// Add, check and delete share one bucket of ten a minute per user on the
// managed gateway: each may cost a provider write.
func TestRemoteClaimChangesShareOneBucket(t *testing.T) {
	f := newOwnDomainFixture(t)
	vars := map[string]string{"domain": ownDomain}
	f.claim(userA, ownDomain, store.ClaimVerified)
	f.providerClaim(t, ownDomain, store.ClaimVerified)
	for i := 0; i < remoteChecksPerMin; i++ {
		var rec *httptest.ResponseRecorder
		switch i % 3 {
		case 0:
			rec = f.add(userA, false, ownDomain)
		case 1:
			rec = f.call(f.cd.VerifyTXT, http.MethodPost, userA, false, vars, nil)
		default:
			rec = f.call(f.cd.Delete, http.MethodDelete, userA, false, map[string]string{"domain": "none.own.net"}, nil)
		}
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("call %d limited: %s", i, rec.Body)
		}
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"add":    f.add(userA, false, ownDomain),
		"verify": f.call(f.cd.VerifyTXT, http.MethodPost, userA, false, vars, nil),
		"delete": f.call(f.cd.Delete, http.MethodDelete, userA, false, vars, nil),
	} {
		if rec.Code != http.StatusTooManyRequests || code(t, rec) != "rate_limited" || !strings.Contains(rec.Body.String(), "try again in a minute") {
			t.Fatalf("%s over the shared bucket: %d %s", name, rec.Code, rec.Body)
		}
	}
	if _, err := f.st.c.GetCustomDomainClaim(userA, ownDomain); err != nil {
		t.Fatal("a limited delete removed the claim")
	}
}

// At most twelve own-domain claim writes a minute leave this Core for all
// users together; the next Add is 429 with no provider write and no claim.
func TestRemoteClaimWritesHaveAPlatformCeiling(t *testing.T) {
	f := newOwnDomainFixture(t)
	n := 0
	for u := 0; n < 12; u++ {
		user := fmt.Sprintf("user-%d", u)
		for d := 0; d < maxPendingProviderClaims && n < 12; d++ {
			if rec := f.add(user, false, fmt.Sprintf("d%d.u%d.own.net", d, u)); rec.Code != 200 {
				t.Fatalf("write %d: %d %s", n, rec.Code, rec.Body)
			}
			n++
		}
	}
	writes := countCalls(f.fake.Calls(), "POST ")
	rec := f.add("late-user", false, "late.own.net")
	if rec.Code != http.StatusTooManyRequests || code(t, rec) != "rate_limited" ||
		!strings.Contains(rec.Body.String(), "Too many own-domain changes on this platform right now. Try again in a minute.") ||
		rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("13th: %d %s (Retry-After %q)", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
	}
	if got := countCalls(f.fake.Calls(), "POST "); got != writes {
		t.Fatalf("the refused add wrote to the provider: %v", f.fake.Calls())
	}
	if _, err := f.st.c.GetCustomDomainClaim("late-user", "late.own.net"); err != store.ErrNoClaim {
		t.Fatal("the refused add left a local claim")
	}
	if q, _ := f.st.HasOpenOutbox("late.own.net", services.OutboxOpDeleteCustomDomain); q {
		t.Fatal("a release was queued for a claim never sent")
	}
}

// A caller whose own-domain cap is none adds nothing, and the provider is not
// asked at all.
func TestRemoteClaimAddRefusedAtCapNone(t *testing.T) {
	zero := int64(0)
	f := newOwnDomainFixture(t)
	f.st.domCaps["user_default"] = &zero
	rec := f.add(userA, false, ownDomain)
	if rec.Code != http.StatusForbidden || code(t, rec) != "custom_domain_cap" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(f.fake.Calls()) != 0 || len(f.st.c.claims) != 0 {
		t.Fatalf("calls %v, claims %v", f.fake.Calls(), f.st.c.claims)
	}
}

// Releases still queued count against a platform bound, so add-delete-add
// cannot stack claims on the provider faster than they drain.
func TestRemoteClaimAddWaitsForQueuedReleases(t *testing.T) {
	f := newOwnDomainFixture(t)
	for i := 0; i < maxOpenProviderReleases; i++ {
		f.st.outbox = append(f.st.outbox, struct{ op, domain string }{services.OutboxOpDeleteCustomDomain, fmt.Sprintf("gone%d.own.net", i)})
	}
	rec := f.add(userA, false, ownDomain)
	if rec.Code != http.StatusConflict || code(t, rec) != "pending_domains_limit" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if countCalls(f.fake.Calls(), "POST ") != 0 || len(f.st.c.claims) != 0 {
		t.Fatalf("calls %v, claims %v", f.fake.Calls(), f.st.c.claims)
	}
	f.st.outbox = f.st.outbox[1:]
	if rec := f.add(userA, false, ownDomain); rec.Code != 200 {
		t.Fatalf("below the bound: %d %s", rec.Code, rec.Body)
	}
}

// A provider call that fails during a check is said as such, with its mapped
// code and message, never as "not found yet".
func TestRemoteClaimVerifyReportsTheProviderFailure(t *testing.T) {
	for _, c := range []struct {
		name, code string
		setup      func(f *ownDomainFixture)
	}{
		{"change_pending", "change_pending", func(f *ownDomainFixture) {
			f.st.outbox = append(f.st.outbox, struct{ op, domain string }{services.OutboxOpDeleteCustomDomain, ownDomain})
		}},
		{"rate_limited", "rate_limited", func(f *ownDomainFixture) {
			f.providerClaim(t, ownDomain, store.ClaimPending)
			f.warm(t)
			f.fake.FailNext(1, 429, "rate_limited", 0)
		}},
		{"provider_key_rejected", "provider_key_rejected", func(f *ownDomainFixture) {
			f.providerClaim(t, ownDomain, store.ClaimPending)
			f.warm(t)
			f.fake.FailNext(1, 401, "unauthorized", 0)
		}},
		{"provider_unreachable", "provider_unreachable", func(f *ownDomainFixture) {
			f.providerClaim(t, ownDomain, store.ClaimPending)
			f.warm(t)
			dead := httptest.NewServer(http.NotFoundHandler())
			dead.Close()
			f.base = dead.URL
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newOwnDomainFixture(t)
			tok := f.claim(userA, ownDomain, store.ClaimPending)
			f.dns[ownDomain] = []string{tok}
			c.setup(f)
			rec := f.call(f.cd.VerifyTXT, http.MethodPost, userA, false, map[string]string{"domain": ownDomain}, nil)
			var out struct {
				Local, Provider, Message string
				ProviderError            map[string]string
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if rec.Code != 200 || out.Local != "verified" || out.Provider != "pending" || out.ProviderError["code"] != c.code {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if strings.Contains(out.Message, "not found yet") || !strings.Contains(out.Message, out.ProviderError["message"]) ||
				!strings.HasPrefix(out.Message, "This platform has verified "+ownDomain+". The gateway provider could not check it: ") {
				t.Fatalf("message %q, providerError %v", out.Message, out.ProviderError)
			}
		})
	}
}

// The route gate checks the server OWNER's claim; a collaborator or an admin
// acting on a tenant's server is told the owner has to prove it.
func TestRemoteOwnDomainRefusalNamesTheOwner(t *testing.T) {
	f := newOwnDomainFixture(t)
	owner := "Add " + ownDomain + " under Your own domains first"
	notOwner := "The owner of this server has to add and verify " + ownDomain + " under their own domains first."
	for _, c := range []struct {
		name, user string
		admin      bool
		want       string
	}{
		{"owner", userA, false, owner},
		{"collaborator", "collab-1", false, notOwner},
		{"admin", "admin-1", true, notOwner},
	} {
		rec := f.createRoute(c.user, c.admin, "7", ownDomain)
		if rec.Code != http.StatusForbidden || code(t, rec) != "domain_unverified" || !strings.Contains(rec.Body.String(), c.want) {
			t.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	// The local claim is there but the provider's is not verified: the owner
	// is still the one to act.
	f.claim(userA, ownDomain, store.ClaimVerified)
	f.providerClaim(t, ownDomain, store.ClaimPending)
	if rec := f.createRoute("collab-1", false, "7", ownDomain); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), notOwner) {
		t.Fatalf("collaborator, provider pending: %d %s", rec.Code, rec.Body)
	}
	if rec := f.createRoute(userA, false, "7", ownDomain); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "Publish its TXT record shown under Your own domains") {
		t.Fatalf("owner, provider pending: %d %s", rec.Code, rec.Body)
	}
}

// Every refusal of Add carries a code, also when the provider's answers could
// not be waited for.
func TestRemoteClaimAddRefusalsCarryACode(t *testing.T) {
	canceled := func(f *ownDomainFixture) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"domain":"`+ownDomain+`"}`))
		ctx, cancel := context.WithCancel(context.WithValue(r.Context(), "userID", userA))
		cancel()
		rec := httptest.NewRecorder()
		f.cd.Add(rec, r.WithContext(ctx))
		return rec
	}
	f := newOwnDomainFixture(t) // the hoster list is not read yet
	if rec := canceled(f); rec.Code != http.StatusInternalServerError || code(t, rec) != "provider_error" {
		t.Fatalf("regions: %d %s", rec.Code, rec.Body)
	}
	f = newOwnDomainFixture(t)
	if _, err := f.gw.d.RemoteGateway.Regions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := canceled(f); rec.Code != http.StatusInternalServerError || code(t, rec) != "provider_error" ||
		!strings.Contains(rec.Body.String(), "Could not load your custom domains") {
		t.Fatalf("claims list: %d %s", rec.Code, rec.Body)
	}
}

// The list is bounded per user on the managed gateway.
func TestRemoteClaimListIsLimited(t *testing.T) {
	f := newOwnDomainFixture(t)
	for i := 0; i < remoteListsPerMin; i++ {
		if rec := f.call(f.cd.List, http.MethodGet, userA, false, nil, nil); rec.Code != 200 {
			t.Fatalf("list %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if rec := f.call(f.cd.List, http.MethodGet, userA, false, nil, nil); rec.Code != http.StatusTooManyRequests || code(t, rec) != "rate_limited" {
		t.Fatalf("over the limit: %d %s", rec.Code, rec.Body)
	}
	if rec := f.call(f.cd.List, http.MethodGet, userB, false, nil, nil); rec.Code != 200 {
		t.Fatalf("another user: %d %s", rec.Code, rec.Body)
	}
}

// With the mode unread, a malformed Add still answers 503 first, as the gate
// in front of the route did before.
func TestClaimAddUnknownModeBeforeTheBody(t *testing.T) {
	f := newOwnDomainFixture(t)
	f.cd.d.GatewayRemote = func() (bool, bool) { return false, false }
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{not json"))
	r = r.WithContext(context.WithValue(r.Context(), "userID", userA))
	rec := httptest.NewRecorder()
	f.cd.Add(rec, r)
	if rec.Code != http.StatusServiceUnavailable || code(t, rec) != "gateway_provider_unknown" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// staleReplica gives replica B (the fixture's gateway) a cached claim list
// without ownDomain, then has replica A add the claim on the provider with
// its TXT value published.
func staleReplica(t *testing.T, f *ownDomainFixture) {
	t.Helper()
	if _, err := f.gw.d.RemoteGateway.ProviderCustomDomains(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := services.NewRemoteGateway(f.st, func() *remotegw.Client { return remotegw.New(f.base, f.fake.Key) })
	if _, err := other.EnsureProviderClaim(context.Background(), ownDomain); err != nil {
		t.Fatal(err)
	}
	f.fake.SetTXTPublished(ownDomain, true)
}

// "Check now" on a replica whose cached list predates the claim re-reads it
// and asks the provider in the same call, once.
func TestRemoteClaimVerifyRereadsAStaleClaimList(t *testing.T) {
	f := newOwnDomainFixture(t)
	f.claim(userA, ownDomain, store.ClaimVerified)
	staleReplica(t, f)
	verifyPath := "POST /custom-domains/" + ownDomain + "/verify-txt"
	before := countCalls(f.fake.Calls(), verifyPath)
	sent := len(f.fake.Calls())
	rec := f.call(f.cd.VerifyTXT, http.MethodPost, userA, false, map[string]string{"domain": ownDomain}, nil)
	var out struct{ Local, Provider string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out.Local != "verified" || out.Provider != "verified" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if n := countCalls(f.fake.Calls(), verifyPath) - before; n != 1 {
		t.Fatalf("provider verify called %d times, want 1: %v", n, f.fake.Calls())
	}
	// The fresh list knew the claim: no second add was written for it.
	for _, c := range f.fake.Calls()[sent:] {
		if c == "POST /custom-domains" {
			t.Fatalf("the stale list made the check add the claim again: %v", f.fake.Calls()[sent:])
		}
	}
}

// The fresh read fails: the answer says so and nothing reads as verified.
func TestRemoteClaimVerifyFreshReadFails(t *testing.T) {
	f := newOwnDomainFixture(t)
	f.claim(userA, ownDomain, store.ClaimVerified)
	staleReplica(t, f)
	f.fake.FailNext(1, 503, "unavailable", 0)
	rec := f.call(f.cd.VerifyTXT, http.MethodPost, userA, false, map[string]string{"domain": ownDomain}, nil)
	var out struct {
		Provider      string
		ProviderError map[string]string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Provider != "pending" || out.ProviderError["code"] == "" || strings.Contains(rec.Body.String(), `"provider":"verified"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if n := countCalls(f.fake.Calls(), "POST /custom-domains/"+ownDomain+"/verify-txt"); n != 0 {
		t.Fatalf("provider verify called without a claim list: %v", f.fake.Calls())
	}
}
