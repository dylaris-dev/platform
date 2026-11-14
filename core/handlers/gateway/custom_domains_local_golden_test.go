package gateway

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
)

// minted TXT tokens are random; the golden bodies carry a placeholder.
var goldenToken = regexp.MustCompile(`dylaris-verify=[0-9a-f]{32}`)

// goldenStep is one call of the local claim flow: who, which handler, and the
// status and body the code answered BEFORE the remote branch existed.
type goldenStep struct {
	name   string
	call   func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder
	status int
	body   string
}

func sessionCall(fn http.HandlerFunc, method, user string, vars map[string]string, body interface{}) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	fn(rec, extReq(method, "/", user, false, vars, body))
	return rec
}

func extCall(fn func(*ExternalGatewayHandler, http.ResponseWriter, *http.Request), f *extFixture, method, user string, vars map[string]string, body interface{}) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	fn(f.h, rec, extReq(method, "/", user, false, vars, body))
	return rec
}

func dom(d string) map[string]string { return map[string]string{"domain": d} }

// Local mode answers every claim call exactly as it did before own domains on
// the managed gateway existed, through the session routes and the provider's
// external API alike, and asks the provider nothing. The bodies were captured
// from the code before that change (Review Focus 3).
func TestCustomDomainHandlersLocalModeUnchanged(t *testing.T) {
	steps := []goldenStep{
		{"session list", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return sessionCall(h.List, "GET", extTenantB, nil, nil)
		}, 200, `{"domains":[{"domain":"b-own.net","state":"verified","attempts":0,"txtName":"_dylaris-verify.b-own.net","txtValue":"txt-of-b"}],"success":true}`},
		{"session add", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return sessionCall(h.Add, "POST", extTenantC, nil, dom("c-own.net"))
		}, 200, `{"domain":{"domain":"c-own.net","state":"pending","attempts":0,"txtName":"_dylaris-verify.c-own.net","txtValue":"<TOKEN>"},"success":true}`},
		{"session add again", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return sessionCall(h.Add, "POST", extTenantC, nil, dom("c-own.net"))
		}, 200, `{"domain":{"domain":"c-own.net","state":"pending","attempts":0,"txtName":"_dylaris-verify.c-own.net","txtValue":"<TOKEN>"},"success":true}`},
		{"session add hoster name", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return sessionCall(h.Add, "POST", extTenantC, nil, dom("x.example.com"))
		}, 400, `{"message":"custom domain may not be a subdomain of a hoster domain (example.com) — use the subdomain picker instead","success":false}`},
		{"session txt-token", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return sessionCall(h.IssueTXTToken, "POST", extTenantA, dom("a-own.net"), nil)
		}, 200, `{"domain":{"domain":"a-own.net","state":"pending","attempts":0,"txtName":"_dylaris-verify.a-own.net","txtValue":"txt-secret-of-tenant-a"},"success":true}`},
		{"session txt-token verified", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return sessionCall(h.IssueTXTToken, "POST", extTenantB, dom("b-own.net"), nil)
		}, 400, `{"message":"That domain is already verified","success":false}`},
		{"session verify not found yet", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return sessionCall(h.VerifyTXT, "POST", extTenantA, dom("a-own.net"), nil)
		}, 409, `{"message":"The TXT record was not found yet. DNS changes can take a few minutes to publish.","success":false}`},
		{"session verify cooldown", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return sessionCall(h.VerifyTXT, "POST", extTenantA, dom("a-own.net"), nil)
		}, 429, `{"code":"verify_cooldown","message":"You checked this domain a moment ago. Try again in 5 minutes.","success":false}`},
		{"session verify verified", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return sessionCall(h.VerifyTXT, "POST", extTenantB, dom("b-own.net"), nil)
		}, 400, `{"message":"That domain is already verified","success":false}`},
		{"session verify no claim", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return sessionCall(h.VerifyTXT, "POST", extTenantA, dom("b-own.net"), nil)
		}, 404, `{"message":"No claim on that domain for your account","success":false}`},
		{"session delete routed", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			seedLinkRoute(t, f.rdb, "b-own.net", services.GatewayRoute{CoreOwned: true, OwnerID: extTenantB,
				TunnelID: "derived-token-link-b", TargetIP: "192.168.1.20", TargetPort: 25565})
			return sessionCall(h.Delete, "DELETE", extTenantB, dom("b-own.net"), nil)
		}, 409, `{"code":"domain_in_use","message":"Delete the routes on b-own.net first.","success":false}`},
		{"session delete no claim", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return sessionCall(h.Delete, "DELETE", extTenantB, dom("nobody.net"), nil)
		}, 404, `{"message":"No claim on that domain for your account","success":false}`},
		{"session delete", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return sessionCall(h.Delete, "DELETE", extTenantC, dom("c-own.net"), nil)
		}, 200, `{"success":true}`},
		{"external list", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return extCall((*ExternalGatewayHandler).ListCustomDomains, f, "GET", extTenantB, nil, nil)
		}, 200, `{"items":[{"attempts":0,"domain":"b-own.net","state":"verified","txtName":"_dylaris-verify.b-own.net","txtValue":"txt-of-b"}],"success":true}`},
		{"external add", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return extCall((*ExternalGatewayHandler).AddCustomDomain, f, "POST", extTenantC, nil, dom("c2-own.net"))
		}, 200, `{"domain":{"domain":"c2-own.net","state":"pending","attempts":0,"txtName":"_dylaris-verify.c2-own.net","txtValue":"<TOKEN>"},"success":true}`},
		{"external txt-token", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return extCall((*ExternalGatewayHandler).IssueCustomDomainTXTToken, f, "POST", extTenantC, dom("c2-own.net"), nil)
		}, 200, `{"domain":{"domain":"c2-own.net","state":"pending","attempts":0,"txtName":"_dylaris-verify.c2-own.net","txtValue":"<TOKEN>"},"success":true}`},
		{"external verify not found yet", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return extCall((*ExternalGatewayHandler).VerifyCustomDomainTXT, f, "POST", extTenantC, dom("c2-own.net"), nil)
		}, 409, `{"code":"conflict","error":"The TXT record was not found yet. DNS changes can take a few minutes to publish.","success":false}`},
		{"external verify cooldown", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return extCall((*ExternalGatewayHandler).VerifyCustomDomainTXT, f, "POST", extTenantC, dom("c2-own.net"), nil)
		}, 429, `{"code":"verify_cooldown","error":"You checked this domain a moment ago. Try again in 5 minutes.","success":false}`},
		{"external delete routed", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return extCall((*ExternalGatewayHandler).DeleteCustomDomain, f, "DELETE", extTenantB, dom("b-own.net"), nil)
		}, 409, `{"code":"domain_in_use","error":"Delete the routes on b-own.net first.","success":false}`},
		{"external delete", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return extCall((*ExternalGatewayHandler).DeleteCustomDomain, f, "DELETE", extTenantC, dom("c2-own.net"), nil)
		}, 200, `{"success":true}`},
		{"session add with own domains off", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			f.st.settings["gateway_custom_domains_enabled"] = "false"
			return sessionCall(h.Add, "POST", extTenantC, nil, dom("off-own.net"))
		}, 409, `{"code":"custom_domains_off","message":"Your own domains are not enabled on this platform.","success":false}`},
		{"external add with own domains off", func(f *extFixture, h *CustomDomainHandler) *httptest.ResponseRecorder {
			return extCall((*ExternalGatewayHandler).AddCustomDomain, f, "POST", extTenantC, nil, dom("off-own.net"))
		}, 409, `{"code":"custom_domains_off","error":"Your own domains are not enabled on this platform.","success":false}`},
	}

	f := newExtFixture(t)
	// The fixture's verify cooldown needs Redis on the session handler too, and
	// the provider must never be asked in local mode.
	fake := remotegwtest.NewFake(t)
	d := Deps{Store: f.st, Redis: f.rdb,
		GatewayRemote: func() (bool, bool) { return false, true },
		RemoteGateway: services.NewRemoteGateway(f.st, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })}
	h := NewCustomDomainHandler(d, noTXT{})
	// The external fixture's handler shares the fixture's Redis for the cooldown.
	f.h.domains.d.Redis = f.rdb

	for _, s := range steps {
		rec := s.call(f, h)
		got := goldenToken.ReplaceAllString(strings.TrimSpace(rec.Body.String()), "<TOKEN>")
		if rec.Code != s.status || got != s.body {
			t.Errorf("%s:\n got %d %s\nwant %d %s", s.name, rec.Code, got, s.status, s.body)
		}
		if testing.Verbose() {
			t.Logf("%s: %s %s", s.name, strconv.Itoa(rec.Code), got)
		}
	}
	if c := fake.Calls(); len(c) != 0 {
		t.Fatalf("local mode asked the provider: %v", c)
	}
}
