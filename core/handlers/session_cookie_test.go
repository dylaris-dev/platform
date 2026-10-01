package handlers

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A non-nil ConnectionState is all r.TLS != nil needs; nothing reads its fields.
var tlsDummy = tls.ConnectionState{}

func cookieByName(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// The attributes are the feature. Every one of them is load-bearing and three
// of them are load-bearing in a way that is invisible until something breaks
// somewhere else.
func TestSessionCookieAttributes(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "https://panel.example.com/api/auth/login", nil)
	r.TLS = &tlsDummy
	setSessionCookie(rec, r, "the-jwt", 3600)

	c := cookieByName(rec, secureSessionCookieName)
	if c == nil {
		t.Fatal("no session cookie was set")
	}
	if !c.HttpOnly {
		t.Error("the session cookie is readable by script, which is the whole thing this changes")
	}
	if !c.Secure {
		t.Error("Secure is off on an https request")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	// Host-only. A Domain would widen the cookie to every subdomain - including
	// the per-tab content hosts that serve a tenant's own container output - and
	// would be rejected outright by the Beam client, which proxies the panel
	// onto its own wails.localhost origin.
	if c.Domain != "" {
		t.Errorf("Domain = %q, want empty (host-only)", c.Domain)
	}

	hint := cookieByName(rec, signedInHintName)
	if hint == nil {
		t.Fatal("no signed-in hint was set; the panel would flash the login screen on every load")
	}
	if hint.HttpOnly {
		t.Error("the hint is HttpOnly, so the panel cannot read it and it does nothing")
	}
	if hint.Value == "the-jwt" {
		t.Fatal("the hint carries the session token; it must hold nothing")
	}
}

// Secure follows the scheme, including through a terminating proxy. Getting this
// wrong in the safe-looking direction is not safe: a Secure cookie on a plain
// http self-host install is dropped by the browser and nobody can log in.
func TestSecureFollowsTheScheme(t *testing.T) {
	tests := []struct {
		name       string
		tls        bool
		forwarded  string
		wantSecure bool
	}{
		{name: "direct https", tls: true, wantSecure: true},
		{name: "behind a terminating proxy", forwarded: "https", wantSecure: true},
		{name: "proxy header casing", forwarded: "HTTPS", wantSecure: true},
		{name: "plain http self-host", wantSecure: false},
		{name: "proxy reports http", forwarded: "http", wantSecure: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://panel.example.com/", nil)
			if tt.tls {
				r.TLS = &tlsDummy
			}
			if tt.forwarded != "" {
				r.Header.Set("X-Forwarded-Proto", tt.forwarded)
			}
			rec := httptest.NewRecorder()
			setSessionCookie(rec, r, "t", 60)
			if got := cookieByName(rec, sessionCookieNameFor(r)).Secure; got != tt.wantSecure {
				t.Errorf("Secure = %v, want %v", got, tt.wantSecure)
			}
		})
	}
}

// Clearing has to actually clear. A cookie rewritten with an empty value but a
// live Max-Age keeps arriving and keeps being rejected, which reads to the panel
// as a broken server rather than as a signed-out state.
func TestClearingUsesANegativeAge(t *testing.T) {
	rec := httptest.NewRecorder()
	clearSessionCookie(rec, httptest.NewRequest(http.MethodPost, "https://panel.example.com/api/auth/logout", nil))
	for _, name := range []string{sessionCookieName, signedInHintName} {
		c := cookieByName(rec, name)
		if c == nil {
			t.Fatalf("%s was not cleared at all", name)
		}
		if c.MaxAge >= 0 {
			t.Errorf("%s MaxAge = %d, want negative", name, c.MaxAge)
		}
		if c.Value != "" {
			t.Errorf("%s still carries %q", name, c.Value)
		}
	}
}

// The CSRF gate, and the reason it exists at all.
//
// SameSite=Lax withholds the cookie from cross-SITE requests. This platform
// deliberately serves tenant container output on hosts under
// TAB_PROXY_HOST_SUFFIX, which share a registrable domain with the panel - so
// those hosts are same-SITE and Lax would happily send the session along. That
// is the exact isolation the tab proxy exists to provide, and it would be
// reopened by moving the session into a cookie without this check.
func TestCookieAuthRefusesAnotherOriginsMutation(t *testing.T) {
	const frontend = "https://panel.example.com"
	tests := []struct {
		name, method, origin string
		want                 bool
	}{
		{name: "the panel itself", method: http.MethodPost, origin: frontend, want: true},
		{name: "the panel, port and case normalised", method: http.MethodPost, origin: "https://Panel.Example.com", want: true},
		// The one that matters: same registrable domain, different origin, and
		// it is a host serving a tenant's own container output.
		{name: "a tab content host", method: http.MethodPost, origin: "https://abc.share.example.com", want: false},
		{name: "the share wrapper", method: http.MethodPost, origin: "https://share.example.com", want: false},
		{name: "an unrelated site", method: http.MethodPost, origin: "https://evil.test", want: false},
		// Absent means an old browser or a non-browser client. A non-browser
		// client should be sending the header, where this gate does not apply.
		{name: "no Origin at all", method: http.MethodPost, origin: "", want: false},
		{name: "wrong scheme", method: http.MethodPost, origin: "http://panel.example.com", want: false},
		// Safe methods carry no side effect to protect.
		{name: "GET from anywhere", method: http.MethodGet, origin: "https://evil.test", want: true},
		{name: "HEAD from anywhere", method: http.MethodHead, origin: "", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, frontend+"/api/servers/1/power", nil)
			r.Host = "panel.example.com"
			r.TLS = &tlsDummy // the request itself arrived over https
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if got := requireSameOriginForCookieAuth(r, frontend); got != tt.want {
				t.Errorf("allowed = %v, want %v", got, tt.want)
			}
		})
	}
}

// FRONTEND_URL is one value, and a Core legitimately answers on several names:
// the public host, localhost during setup, a LAN address. A page addressing the
// host it was served from is the panel talking to its own origin.
func TestCookieAuthAcceptsTheHostItWasAddressedTo(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://lan.internal/api/servers/1/power", nil)
	r.Host = "lan.internal"
	r.Header.Set("Origin", "http://lan.internal")
	if !requireSameOriginForCookieAuth(r, "https://panel.example.com") {
		t.Error("a page addressing the host it came from was refused")
	}
	// And that leniency is not a hole: a page elsewhere still cannot match,
	// because Origin says where the PAGE came from, not where it is sending.
	r.Header.Set("Origin", "https://evil.test")
	if requireSameOriginForCookieAuth(r, "https://panel.example.com") {
		t.Error("a foreign origin matched the request host")
	}
}

// The Beam desktop client, which is the case neither of the two rules above can
// reach - and it took a 403 on every mutation in the app to notice.
//
// Beam proxies the panel onto its own wails.localhost origin and then REWRITES
// the Host to the real panel's, deliberately: the panel's edge routes on Host,
// so it has to be the real one (gateway beam/app/proxy.go). Core therefore sees
// Host = the panel and Origin = wails, and neither FRONTEND_URL nor the request
// host matches. Every POST from the desktop app was refused.
//
// Accepting the origin ALONE is safe here, and only here, because of what this
// gate is for: a page in a browser being used as a confused deputy. A browser
// sets Origin itself, so no page can claim to be the webview unless it is. A
// non-browser caller can forge any origin it likes - but one holding the session
// cookie holds the JWT, and would simply send it as a Bearer, where this gate
// does not apply at all. It never defended against them.
func TestCookieAuthAcceptsTheBeamWebview(t *testing.T) {
	tests := []struct {
		name, origin string
		want         bool
	}{
		{name: "windows webview", origin: "http://wails.localhost", want: true},
		{name: "mac and linux scheme", origin: "wails://wails.localhost", want: true},
		{name: "an attacker-registered lookalike", origin: "http://notwails.localhost", want: false},
		{name: "a subdomain of it", origin: "http://x.wails.localhost", want: false},
		{name: "an unrelated site", origin: "https://evil.test", want: false},
		// The one the tab proxy exists to stop: same registrable domain as the
		// panel, serving a tenant's own container output.
		{name: "a tab content host", origin: "https://abc.share.example.com", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The Host beam actually sends: the real panel's, not the webview's.
			r := httptest.NewRequest(http.MethodPost, "https://panel.example.com/api/servers/1/power", nil)
			r.Host = "panel.example.com"
			r.TLS = &tlsDummy
			r.Header.Set("Origin", tt.origin)
			if got := requireSameOriginForCookieAuth(r, "https://panel.example.com"); got != tt.want {
				t.Errorf("allowed = %v, want %v", got, tt.want)
			}
		})
	}
}

// The bearer-copy endpoint is guarded by a PAIR of conditions, and the test is
// here because either alone would be forgeable from the wrong side.
//
// A browser sets Origin itself, so a page cannot claim to be the Beam webview -
// but a non-browser caller can send any Origin it likes. Host is whatever the
// caller resolved, and wails.localhost resolves to loopback for everyone - but a
// browser page cannot choose which Host its own origin's requests carry.
// Requiring both means a caller has to simultaneously BE the webview and be
// addressing it.
func TestWailsOriginNeedsBothTheOriginAndTheHost(t *testing.T) {
	tests := []struct {
		name, host, origin string
		want               bool
	}{
		{name: "windows webview", host: "wails.localhost", origin: "http://wails.localhost", want: true},
		{name: "mac and linux scheme", host: "wails.localhost", origin: "wails://wails.localhost", want: true},
		{name: "webview with a port", host: "wails.localhost:34115", origin: "http://wails.localhost", want: true},
		// A page in a normal browser: it cannot set Origin, so this shape is
		// only reachable by a non-browser caller - which the Host then stops.
		{name: "right origin, real panel host", host: "panel.example.com", origin: "http://wails.localhost", want: false},
		// A caller who resolved wails.localhost but is a page somewhere else.
		{name: "right host, foreign origin", host: "wails.localhost", origin: "https://evil.test", want: false},
		{name: "neither", host: "panel.example.com", origin: "https://panel.example.com", want: false},
		{name: "no origin at all", host: "wails.localhost", origin: "", want: false},
		// Not a suffix match: an attacker-registered lookalike must not pass.
		{name: "lookalike host", host: "notwails.localhost", origin: "http://notwails.localhost", want: false},
		{name: "subdomain of the webview host", host: "x.wails.localhost", origin: "http://x.wails.localhost", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://x/api/auth/session-token", nil)
			r.Host = tt.host
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if got := isWailsOrigin(r); got != tt.want {
				t.Errorf("isWailsOrigin = %v, want %v", got, tt.want)
			}
		})
	}
}

// A tenant's tab content is served from a sibling host under the panel's own
// domain, and its script could set dylaris_session with that domain: the
// browser sent it ahead of the real cookie, running the victim's requests in
// the attacker's account. Over HTTPS the session lives under the __Host- name,
// which no other host can set, and the plain name is not read at all.
func TestAThrownSessionCookieIsIgnoredOverHTTPS(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://panel.example.com/api/servers", nil)
	r.TLS = &tlsDummy
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "attackers-jwt"})
	if got := sessionTokenFromCookie(r); got != "" {
		t.Fatalf("the thrown cookie was read over https: %q", got)
	}
	r.AddCookie(&http.Cookie{Name: secureSessionCookieName, Value: "victims-jwt"})
	if got := sessionTokenFromCookie(r); got != "victims-jwt" {
		t.Fatalf("session = %q, want the __Host- cookie", got)
	}

	plain := httptest.NewRequest(http.MethodGet, "http://panel.example.com/api/servers", nil)
	plain.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "selfhost-jwt"})
	if got := sessionTokenFromCookie(plain); got != "selfhost-jwt" {
		t.Fatalf("a plain-http install lost its session cookie: %q", got)
	}
}

// HSTS was set nowhere and nosniff only on a few responses.
func TestSecurityHeadersOnPanelAndAPIResponses(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	r := httptest.NewRequest(http.MethodGet, "https://panel.example.com/api/servers", nil)
	r.TLS = &tlsDummy
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Header().Get("Strict-Transport-Security") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers = %v, want HSTS and nosniff over https", rec.Header())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://panel.example.com/", nil))
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS was sent over plain http, which a self-host on http could never undo")
	}
}
