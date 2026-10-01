package handlers

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// The session as a cookie.
//
// It carries the SAME JWT the Authorization header carried, so nothing about
// what a session IS changes: same claims, same signing key, same issuer
// allowlist, same password-fingerprint check, same authorization read from the
// row. Only the transport is different, and that difference is the whole point -
// a token in localStorage is readable by any script that manages to run on the
// page, and an HttpOnly cookie is not.
//
// This became possible when Core started serving the panel: the pages and /api
// are one origin now, which is what a cookie needs. It was not worth doing while
// they were two.

const sessionCookieName = "dylaris_session"

// secureSessionCookieName is the session cookie's name over HTTPS. The __Host-
// prefix makes the browser refuse any cookie of that name that carries a
// Domain, is not Secure or is not Path=/ - so nothing but this host can set it.
//
// The plain name could be set from a sibling host. Tenant tab content is
// served from <label>.<suffix>, under the same registrable domain as the panel
// by design, and a container's script could write dylaris_session with
// Domain=<that domain> and a longer path: the browser sends it ahead of the
// real one, so the victim's requests ran in the attacker's account, or a
// junk value locked them out until they cleared the parent domain's cookies.
// Over HTTPS the plain name is no longer read at all; a fallback to it would
// keep the hole open. Plain-HTTP installs cannot use the prefix and keep it.
const secureSessionCookieName = "__Host-" + sessionCookieName

// sessionCookieNameFor is the session cookie's name on this request's scheme.
func sessionCookieNameFor(r *http.Request) string {
	if requestIsHTTPS(r) {
		return secureSessionCookieName
	}
	return sessionCookieName
}

// signedInHintName is a companion cookie carrying no secret at all - just the
// fact that a session exists.
//
// The panel cannot read the session cookie (that is the point), and it needs
// SOME way to tell "signed in" from "signed out" before its first API call, or
// every page load flashes the login screen. A separate readable flag is the
// standard answer and is safe precisely because forging it grants nothing: the
// API still refuses without the real cookie, and the panel's only reaction to a
// forged flag is to render a page whose data requests then 401.
const signedInHintName = "dylaris_signed_in"

// setSessionCookie installs the session and its readable hint.
//
// Host-only: no Domain attribute, deliberately. A Domain would widen the cookie
// to every subdomain - including the per-tab content hosts under
// TAB_PROXY_HOST_SUFFIX, which serve a tenant's own container output. It would
// also break the Beam desktop client, which proxies the panel onto its own
// wails.localhost origin: a cookie scoped to the real panel domain is simply
// rejected by the browser there, while a host-only one is stored against
// whatever host the response arrived on and keeps working.
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAgeSeconds int) {
	secure := requestIsHTTPS(r)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieNameFor(r),
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		// Lax, not Strict. Strict would drop the cookie on a top-level
		// navigation that came from anywhere else - the link in a verification
		// or password-reset email is exactly that - so the user would land on
		// the panel logged out and have to navigate again for no visible
		// reason. Lax still withholds it from cross-site POST and fetch, and
		// the Origin check in requireSameOriginForCookieAuth covers what Lax
		// does not (see there: same-SITE is not the same as same-origin, and
		// this platform deliberately hosts tenant content same-site).
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAgeSeconds,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     signedInHintName,
		Value:    "1",
		Path:     "/",
		HttpOnly: false, // readable on purpose; it holds nothing
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAgeSeconds,
	})
}

// clearSessionCookie removes both, on logout and whenever a session is refused.
//
// MaxAge -1 rather than an empty value with the same lifetime: a browser only
// drops a cookie on a negative age, and a cleared-but-present cookie would keep
// arriving and keep being rejected, which reads to the panel as a server fault
// rather than as a signed-out state.
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	secure := requestIsHTTPS(r)
	names := []string{sessionCookieName, signedInHintName}
	if secure {
		names = append(names, secureSessionCookieName)
	}
	for _, name := range names {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			HttpOnly: name != signedInHintName,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
	}
}

// sessionTokenFromCookie returns the JWT the browser sent, or "".
func sessionTokenFromCookie(r *http.Request) string {
	c, err := r.Cookie(sessionCookieNameFor(r))
	if err != nil || c == nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}

// requestIsHTTPS decides the Secure attribute. TLS may be terminated at a
// reverse proxy, which is the normal deployment, so the forwarded scheme counts
// too - the same test the tab-proxy cookie has always used.
//
// Getting this wrong in the safe-looking direction breaks the product: a Secure
// cookie on a plain-http self-host install is dropped by the browser, and the
// user simply cannot log in.
func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// requireSameOriginForCookieAuth is the CSRF gate, and it exists because
// SameSite is not enough HERE specifically.
//
// SameSite=Lax withholds the cookie from cross-SITE requests. This platform
// deliberately serves tenant container output on hosts under
// TAB_PROXY_HOST_SUFFIX, which share a registrable domain with the panel - so
// those hosts are same-SITE, and a page there would carry the session cookie on
// a request to /api. That is the exact scenario the tab-proxy origin isolation
// was built for, and it would be reopened by moving the session into a cookie
// without this.
//
// The rule: a request authenticated BY COOKIE that changes state must name an
// Origin, and it must be the panel's own. Requests carrying an Authorization
// header never reach this - an API key or an explicit bearer is not ambient
// authority and cannot be triggered by a page the caller did not write.
//
// A missing Origin is refused rather than allowed. Browsers send it on every
// cross-origin request and on same-origin POST, so absent means either a very
// old browser or a non-browser client - and a non-browser client should be using
// the header, where this check does not apply.
func requireSameOriginForCookieAuth(r *http.Request, frontendURL string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		// Safe methods. Core routes every mutation behind an explicit verb, so
		// a GET carries no side effect to protect.
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	return sameOrigin(origin, frontendURL) || originMatchesHost(origin, r) || isWailsWebviewOrigin(origin)
}

// isWailsWebviewOrigin accepts the Beam desktop client's own origin.
//
// It is a third rule because neither of the other two can reach that case. Beam
// proxies the panel onto wails.localhost and then REWRITES the Host to the real
// panel's - deliberately, because the panel's edge routes on Host. Core
// therefore sees the panel as the Host and wails as the Origin, which matches
// neither FRONTEND_URL nor the request host, and every mutation from the desktop
// app was refused.
//
// The ORIGIN alone is enough here, and the reason is what this gate defends
// against: a page in a browser used as a confused deputy. A browser sets Origin
// itself, so no page can claim to be the webview unless it is one. A non-browser
// caller can forge any origin - but one holding the session cookie holds the JWT
// inside it, and would send it as a Bearer, where this gate does not apply at
// all. It never stood between that caller and anything.
//
// Exact host, not a suffix: notwails.localhost and x.wails.localhost are other
// people's origins.
func isWailsWebviewOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), wailsHost)
}

// sameOrigin compares an Origin header against a configured URL by scheme, host
// and port, ignoring anything after.
func sameOrigin(origin, configured string) bool {
	a, err := url.Parse(origin)
	if err != nil || a.Host == "" {
		return false
	}
	b, err := url.Parse(strings.TrimSpace(configured))
	if err != nil || b.Host == "" {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// originMatchesHost accepts an Origin that is the host this request was actually
// addressed to.
//
// FRONTEND_URL is one value and a Core can legitimately answer on several names:
// the operator's public hostname, localhost during setup, a LAN address, and the
// wails.localhost the Beam desktop client proxies onto. All of those are the
// panel talking to its own origin, which is precisely what this gate is for.
//
// It is not a hole: the comparison is against the Host the browser used, so a
// page on another origin still cannot match - its Origin header says where the
// page came from, not where it is sending.
func originMatchesHost(origin string, r *http.Request) bool {
	a, err := url.Parse(origin)
	if err != nil || a.Host == "" {
		return false
	}
	// The SCHEME counts too. http://panel.example.com and
	// https://panel.example.com are different origins, and treating them as one
	// would let a plain-http page act on an https session wherever both are
	// reachable - which is every deployment that has not yet turned on a
	// redirect, and every one behind a proxy that answers both.
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	return strings.EqualFold(a.Scheme, scheme) && strings.EqualFold(a.Host, r.Host)
}

// wailsHost is the hostname the Beam desktop client's webview runs on. Windows
// serves it as http://wails.localhost, macOS and Linux as wails://wails.localhost.
const wailsHost = "wails.localhost"

// isWailsOrigin reports whether a request came from inside the Beam desktop
// client, by BOTH the origin the page claims and the host it addressed.
//
// Both, because either alone is forgeable from the wrong side: a browser page
// can send any Origin it likes only if it is not a browser at all (browsers set
// it themselves), while Host is whatever DNS the caller resolved - and
// wails.localhost resolves to loopback for everyone. Requiring the pair means a
// caller must simultaneously BE the webview and be addressing it.
func isWailsOrigin(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if !strings.EqualFold(host, wailsHost) {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if strings.HasPrefix(strings.ToLower(origin), "wails://") {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Hostname(), wailsHost)
}
