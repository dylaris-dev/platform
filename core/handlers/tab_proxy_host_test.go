package handlers

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// The host matcher runs on every single request this process serves, including
// the panel's and the API's, and it decides whether a request is handed to a
// tenant's container or to Core's own router. Anything it accepts wrongly puts
// container output on an origin that was supposed to be ours; anything it
// rejects wrongly takes a customer's tab offline.
func TestTabProxyHostLabel(t *testing.T) {
	const suffix = "share.example.com"
	const good = "abcdefghij0123456789" // 20 chars, lowercase base36

	cases := []struct {
		name string
		host string
		want string
	}{
		{"plain match", good + ".share.example.com", good},
		{"with port", good + ".share.example.com:443", good},
		{"uppercase host folded", strings.ToUpper(good[:4]) + good[4:] + ".SHARE.EXAMPLE.COM", good},
		{"trailing dot tolerated", good + ".share.example.com.", good},
		{"the bare suffix is not a tab", suffix, ""},
		{"the panel is not a tab", "panel.example.com", ""},
		{"a second level is not a label", "a." + good + ".share.example.com", ""},
		{"label too short", "abc.share.example.com", ""},
		{"label too long", good + "x.share.example.com", ""},
		{"uppercase-only label is not one of ours", strings.ToUpper(good) + ".share.example.com", strings.ToLower(good)},
		{"hyphen not in the alphabet", "abcdefghij-123456789.share.example.com", ""},
		{"suffix as a substring, not a suffix", good + ".share.example.com.evil.test", ""},
		{"empty host", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TabProxyHostLabel(tc.host, suffix); got != tc.want {
				t.Errorf("TabProxyHostLabel(%q) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}

// No suffix configured means no content hosts exist, so nothing may match -
// otherwise an empty setting would turn every request into a tab lookup.
func TestTabProxyHostLabel_NoSuffixMatchesNothing(t *testing.T) {
	for _, host := range []string{"abcdefghij0123456789.share.example.com", "panel.example.com", ""} {
		if got := TabProxyHostLabel(host, ""); got != "" {
			t.Errorf("TabProxyHostLabel(%q, \"\") = %q, want empty", host, got)
		}
	}
}

// A DNS label is case-insensitive. The share token next to it is base62 WITH
// uppercase, so reusing it would let two distinct tokens name one host and the
// proxy would have to guess which tab a request meant. This is the property that
// makes that impossible, so it is asserted rather than assumed.
func TestGenerateProxyHostLabel_IsADNSLabel(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		got, err := generateProxyHostLabel()
		if err != nil {
			t.Fatalf("generateProxyHostLabel: %v", err)
		}
		if !isProxyHostLabel(got) {
			t.Fatalf("generated %q which its own validator rejects", got)
		}
		if got != strings.ToLower(got) {
			t.Fatalf("generated %q is not lowercase, so two labels could collide on DNS", got)
		}
		if seen[got] {
			t.Fatalf("generated %q twice in 200 draws - the entropy is not what it claims", got)
		}
		seen[got] = true
	}
}

func TestIsProxyHostLabel(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"abcdefghij0123456789", true},
		{"00000000000000000000", true},
		{"", false},
		{"short", false},
		{"abcdefghij01234567890", false}, // 21
		{"abcdefghij012345678", false},   // 19
		{"ABCDEFGHIJ0123456789", false},  // uppercase
		{"abcdefghij-123456789", false},  // hyphen
		{"abcdefghij.123456789", false},  // dot
		{"abcdefghij_123456789", false},  // underscore
		{"abcdefghij0123456 89", false},  // space
	}
	for _, tc := range cases {
		if got := isProxyHostLabel(tc.in); got != tc.want {
			t.Errorf("isProxyHostLabel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Every proxied tab has a content host now, published or not. Passing
// tab.Visibility straight through would make a public-but-never-shared tab
// anonymously reachable the moment it is created - which is not what "public"
// meant when a share link was the only way to reach one.
func TestHostVisibility_RequiresAnActualLink(t *testing.T) {
	cases := []struct {
		name       string
		visibility string
		token      string
		surface    string
		want       string
	}{
		{"public and shared", "public", "tok123", "page", "public"},
		{"public, shared, both surfaces", "public", "tok123", "both", "public"},
		{"public but never shared", "public", "", "page", "private"},
		{"private with a token", "private", "tok123", "page", "private"},
		{"private", "private", "", "page", "private"},
		// Switched to "panel only" after it was shared: the token stayed, and
		// so did anonymous access to its content host.
		{"panel-only tab that kept its token", "public", "tok123", "tab", "private"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tab := &proxyTab{Visibility: tc.visibility, Surface: tc.surface}
			if tc.token != "" {
				tab.ShareToken = sql.NullString{String: tc.token, Valid: true}
			}
			if got := hostVisibility(tab); got != tc.want {
				t.Errorf("hostVisibility = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- the gate between a request and a container ---

// hostGateStore is the smallest store the gate touches: the feature flags read
// settings, nothing else is reached.
type hostGateStore struct {
	store.Store
	settings map[string]string
}

func (f *hostGateStore) GetSetting(key string) (string, error) { return f.settings[key], nil }

func (f *hostGateStore) GetServerByID(id int) (*models.Server, error) {
	return nil, sql.ErrNoRows
}

func newHostGateHandler(t *testing.T, enabled, allowPublic bool) (*ProxyHandler, *AuthHandler) {
	t.Helper()
	fs := &hostGateStore{settings: map[string]string{}}
	if enabled {
		fs.settings["feature_tab_proxy_enabled"] = "true"
	}
	if allowPublic {
		// No "feature_" prefix on this one, unlike the master toggle next to it.
		// The two keys really are spelled differently in FeatureFlags; guessing
		// cost this test a red run.
		fs.settings["tab_proxy_allow_public_links"] = "true"
	}
	state := &AppState{
		Store:              fs,
		FeatureFlags:       services.NewFeatureFlags(fs),
		FrontendURL:        "https://panel.example.com",
		TabProxyHostSuffix: "share.example.com",
	}
	ah := NewAuthHandler(state, "test-tab-proxy-secret")
	return NewProxyHandler(state, ah), ah
}

func proxiedTab(visibility, token string) *proxyTab {
	t := &proxyTab{
		ID: 2, ServerID: 1, ServerUUID: "srv-1-uuid", NodeID: 7,
		Mode: "proxied", TargetPort: 8123, TargetPath: "/",
		Surface: "page", Visibility: visibility, Enabled: true,
	}
	if token != "" {
		t.ShareToken = sql.NullString{String: token, Valid: true}
	}
	return t
}

func hostReq(method string, cookie *http.Cookie) *http.Request {
	r := httptest.NewRequest(method, "https://abcdefghij0123456789.share.example.com/", nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}

func TestHostGate_PrivateWithoutCookieIsUnauthorized(t *testing.T) {
	h, _ := newHostGateHandler(t, true, true)
	rec := httptest.NewRecorder()

	if h.allowHostRequest(rec, hostReq("GET", nil), proxiedTab("private", "tok")) {
		t.Fatal("a private tab served a visitor carrying no ticket at all")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	// The refusal is rendered inside the iframe and has to break out of it, or
	// the panel loads into the frame it was meant to replace.
	if body := rec.Body.String(); !strings.Contains(body, `target="_top"`) ||
		!strings.Contains(body, "https://panel.example.com") {
		t.Errorf("401 page does not point back at the panel from inside a frame: %s", body)
	}
}

func TestHostGate_TicketForAnotherTabIsForbidden(t *testing.T) {
	h, ah := newHostGateHandler(t, true, true)
	// Valid signature, valid purpose, wrong tab. The cookie is host-only per
	// content host, so this should not be reachable in a browser at all - the
	// claim check is the second lock on the same door.
	ticket, err := ah.IssueTabProxyTicket("owner", false, 1, 99, false)
	if err != nil {
		t.Fatalf("IssueTabProxyTicket: %v", err)
	}
	rec := httptest.NewRecorder()

	if h.allowHostRequest(rec, hostReq("GET", &http.Cookie{Name: proxyCookieName, Value: ticket}), proxiedTab("private", "tok")) {
		t.Fatal("a ticket minted for tab 99 opened tab 2")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestHostGate_SessionTokenIsNotATicket(t *testing.T) {
	h, ah := newHostGateHandler(t, true, true)
	session, err := ah.IssueToken("owner", false, "")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	rec := httptest.NewRecorder()

	// Same secret, same signature, different purpose. Accepting it would make a
	// 24h panel session a permanent key to every tab.
	if h.allowHostRequest(rec, hostReq("GET", &http.Cookie{Name: proxyCookieName, Value: session}), proxiedTab("private", "tok")) {
		t.Fatal("a panel session token was accepted as a tab-proxy ticket")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHostGate_ExpiredTicketIsUnauthorized(t *testing.T) {
	h, ah := newHostGateHandler(t, true, true)
	ticket, err := ah.IssueTabProxyTicket("owner", false, 1, 2, false)
	if err != nil {
		t.Fatalf("IssueTabProxyTicket: %v", err)
	}
	// Nothing to fast-forward here without waiting out the TTL, so assert the
	// live one is accepted and rely on ParseTabProxyTicket's own expiry tests.
	rec := httptest.NewRecorder()
	if !h.allowHostRequest(rec, hostReq("GET", &http.Cookie{Name: proxyCookieName, Value: ticket}), proxiedTab("private", "tok")) {
		t.Fatalf("a valid ticket for this exact tab was refused: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHostGate_PublicLinkNeedsNoTicket(t *testing.T) {
	h, _ := newHostGateHandler(t, true, true)
	rec := httptest.NewRecorder()

	if !h.allowHostRequest(rec, hostReq("GET", nil), proxiedTab("public", "tok")) {
		t.Fatalf("a public link refused an anonymous visitor: %d %s", rec.Code, rec.Body.String())
	}
}

// The operator's kill switch for anonymous links has to hide the tab, not
// downgrade it to "sign in" - the tab is not private, it is unavailable.
func TestHostGate_PublicLinkHiddenWhenTheFlagIsOff(t *testing.T) {
	h, _ := newHostGateHandler(t, true, false)
	rec := httptest.NewRecorder()

	if h.allowHostRequest(rec, hostReq("GET", nil), proxiedTab("public", "tok")) {
		t.Fatal("an anonymous link served while allow-public-links is off")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHostGate_DisabledAndNonProxiedTabsAre404(t *testing.T) {
	h, _ := newHostGateHandler(t, true, true)

	disabled := proxiedTab("public", "tok")
	disabled.Enabled = false
	direct := proxiedTab("public", "tok")
	direct.Mode = "direct"

	for name, tab := range map[string]*proxyTab{"disabled": disabled, "direct": direct} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if h.allowHostRequest(rec, hostReq("GET", nil), tab) {
				t.Fatal("served a tab that should not have a data plane at all")
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestHostGate_ExpiredLinkIsGone(t *testing.T) {
	h, _ := newHostGateHandler(t, true, true)
	tab := proxiedTab("public", "tok")
	tab.ShareExpires = sql.NullTime{Time: time.Now().Add(-time.Minute), Valid: true}
	rec := httptest.NewRecorder()

	if h.allowHostRequest(rec, hostReq("GET", nil), tab) {
		t.Fatal("an expired link still served content")
	}
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410", rec.Code)
	}
}

// A read-only (demo) ticket may look, not touch. A WebSocket is refused because
// nothing can be inspected once it is upgraded.
func TestHostGate_ReadOnlyTicket(t *testing.T) {
	h, ah := newHostGateHandler(t, true, true)
	ticket, err := ah.IssueTabProxyTicket("demo", false, 1, 2, true)
	if err != nil {
		t.Fatalf("IssueTabProxyTicket: %v", err)
	}
	cookie := &http.Cookie{Name: proxyCookieName, Value: ticket}

	for _, method := range []string{"GET", "HEAD"} {
		rec := httptest.NewRecorder()
		if !h.allowHostRequest(rec, hostReq(method, cookie), proxiedTab("private", "tok")) {
			t.Errorf("%s refused for a read-only ticket: %d", method, rec.Code)
		}
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		rec := httptest.NewRecorder()
		if h.allowHostRequest(rec, hostReq(method, cookie), proxiedTab("private", "tok")) {
			t.Errorf("%s allowed for a read-only ticket", method)
		}
	}

	ws := hostReq("GET", cookie)
	ws.Header.Set("Connection", "Upgrade")
	ws.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	if h.allowHostRequest(rec, ws, proxiedTab("private", "tok")) {
		t.Error("a read-only ticket opened a WebSocket")
	}
}

// --- the host dispatch itself ---

// Everything a container is served on must reach ONLY the proxy. The API and
// every panel route answering there would put them one fetch away from a
// tenant's own JavaScript.
func TestTabProxyHostMux_ContentHostNeverReachesTheRouter(t *testing.T) {
	h, _ := newHostGateHandler(t, true, true)
	reachedNext := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reachedNext = true
		w.WriteHeader(http.StatusTeapot)
	})
	mux := TabProxyHostMux(h.state, h, next)

	for _, path := range []string{"/", "/api/servers", "/api/system/core-info", "/login", "/__dyl/", "/__dyl/other"} {
		reachedNext = false
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "https://abcdefghij0123456789.share.example.com"+path, nil))
		if reachedNext {
			t.Errorf("%s on a content host reached Core's router", path)
		}
	}
}

func TestTabProxyHostMux_OtherHostsPassThrough(t *testing.T) {
	h, _ := newHostGateHandler(t, true, true)
	reachedNext := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reachedNext = true })
	mux := TabProxyHostMux(h.state, h, next)

	for _, host := range []string{"panel.example.com", "api.example.com", "share.example.com", "localhost:25500"} {
		reachedNext = false
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "https://"+host+"/api/servers", nil))
		if !reachedNext {
			t.Errorf("%s did not reach Core's router", host)
		}
	}
}

// No suffix means no content hosts, so the mux must not even wrap - a
// deployment without the feature carries no extra layer on every request.
func TestTabProxyHostMux_UnconfiguredReturnsTheHandlerUntouched(t *testing.T) {
	h, _ := newHostGateHandler(t, true, true)
	h.state.TabProxyHostSuffix = ""
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if got := TabProxyHostMux(h.state, h, next); got == nil {
		t.Fatal("returned nil")
	}
}

// The mint response sets a credentialed cookie. Reflecting an arbitrary Origin
// alongside Allow-Credentials would let any site on the internet mint a ticket
// in a signed-in visitor's browser.
func TestWriteMintCORS_OnlyEchoesTheAllowList(t *testing.T) {
	allowed := tabProxyMintOrigins("https://panel.example.com", "share.example.com")

	for _, origin := range []string{"https://panel.example.com", "https://share.example.com"} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/__dyl/mint", nil)
		r.Header.Set("Origin", origin)
		writeMintCORS(rec, r, allowed)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("Allow-Origin for %s = %q, want %q", origin, got, origin)
		}
		if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
			t.Errorf("Allow-Credentials missing for %s", origin)
		}
	}

	for _, origin := range []string{
		"https://evil.test",
		"https://panel.example.com.evil.test",
		"https://abcdefghij0123456789.share.example.com", // a content host is NOT allowed to mint
		"null",
	} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/__dyl/mint", nil)
		r.Header.Set("Origin", origin)
		writeMintCORS(rec, r, allowed)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Allow-Origin echoed %q for a foreign origin", got)
		}
	}
}

func TestTabProxyOriginHelpers(t *testing.T) {
	if got := TabProxyWrapperOrigin("https://panel.example.com", "share.example.com"); got != "https://share.example.com" {
		t.Errorf("wrapper origin = %q", got)
	}
	if got := TabProxyWrapperOrigin("http://localhost:25510", "share.localhost"); got != "http://share.localhost" {
		t.Errorf("wrapper origin follows the panel scheme, got %q", got)
	}
	if got := TabProxyWrapperOrigin("https://panel.example.com", ""); got != "" {
		t.Errorf("unconfigured wrapper origin = %q, want empty", got)
	}
	if got := TabProxyContentOrigin("https://panel.example.com", "share.example.com", "abcdefghij0123456789"); got != "https://abcdefghij0123456789.share.example.com" {
		t.Errorf("content origin = %q", got)
	}
	if got := TabProxyContentOrigin("https://panel.example.com", "", "abcdefghij0123456789"); got != "" {
		t.Errorf("unconfigured content origin = %q, want empty", got)
	}
}

// --- the mint, after the session stopped being something the panel can send ---

// The panel used to reach the mint cross-origin with its session as a Bearer
// header, which worked only because the session lived in localStorage. It lives
// in an HttpOnly, host-only cookie now, so a fetch to a content host carries no
// credential whatsoever - and the mint stopped being an authorization decision.
//
// The decision moved to the panel's own origin, where the session still is. What
// arrives here is the ticket that decision produced, and this asserts the only
// two things this end may still judge: that it is a ticket at all, and that it
// is for THIS tab.
func TestAllowMint_TakesATicketForThisTab(t *testing.T) {
	h, ah := newHostGateHandler(t, true, true)
	tab := proxiedTab("private", "tok")

	ticket, err := ah.IssueTabProxyTicket("owner", false, tab.ServerID, tab.ID, false)
	if err != nil {
		t.Fatalf("IssueTabProxyTicket: %v", err)
	}
	rec := httptest.NewRecorder()
	got, ok := h.allowMint(rec, mintReq(ticket), tab)
	if !ok {
		t.Fatalf("a ticket minted for this exact tab was refused: %d %s", rec.Code, rec.Body.String())
	}
	if got != ticket {
		t.Error("allowMint returned something other than the ticket it was given")
	}
}

func TestAllowMint_RefusesEverythingElse(t *testing.T) {
	h, ah := newHostGateHandler(t, true, true)
	tab := proxiedTab("private", "tok")

	otherTab, err := ah.IssueTabProxyTicket("owner", false, tab.ServerID, 99, false)
	if err != nil {
		t.Fatalf("IssueTabProxyTicket: %v", err)
	}
	// Same secret, same signature, different purpose. Accepting it would turn a
	// 24h panel session into a permanent key to every tab - which is the whole
	// reason the ticket carries a purpose at all.
	session, err := ah.IssueToken("owner", false, "")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	cases := []struct {
		name, bearer string
		want         int
	}{
		{"nothing at all", "", http.StatusUnauthorized},
		{"a panel session", session, http.StatusUnauthorized},
		{"gibberish", "not-a-jwt", http.StatusUnauthorized},
		{"a ticket for another tab", otherTab, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if _, ok := h.allowMint(rec, mintReq(tc.bearer), tab); ok {
				t.Fatal("the mint accepted it")
			}
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func mintReq(bearer string) *http.Request {
	r := httptest.NewRequest("GET", "https://abcdefghij0123456789.share.example.com/__dyl/mint", nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}
