package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// beamConnectExtra returns the extra connect-src sources for the proxied Panel:
// the configured Panel origin AND the configured Core API origin (each
// scheme://host), space-prefixed and de-duplicated. The Panel is proxied on the
// wails:// origin, so same-origin /api is already covered by 'self'; these entries
// cover a Panel that fetches an ABSOLUTE API URL - a deployment that still puts
// the API on a second hostname, which 'self' and the Panel origin alone would
// not permit. The stock configuration is same-origin and needs neither. Both inputs are optional: an unset
// or unparseable URL is skipped, and "" is returned when neither yields an origin.
// No vendor host is hardcoded - the origins come from the operator's configured /
// build-time Panel and API URLs.
func beamConnectExtra(panelURL, apiURL string) string {
	seen := map[string]bool{}
	extra := ""
	for _, raw := range []string{panelURL, apiURL} {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue
		}
		origin := u.Scheme + "://" + u.Host
		if seen[origin] {
			continue
		}
		seen[origin] = true
		extra += " " + origin
	}
	return extra
}

// beamPanelCSP is the Content-Security-Policy beam sets on proxied Panel
// HTML, when the Panel sent none of its own. A current Core always sends a
// nonce-strict policy and beamCSPForPanel carries that nonce through instead,
// so this is the fallback for an older Core rather than the usual path. It is pragmatic, not nonce-strict: 'unsafe-inline' on script-src
// is a deliberate concession so Next.js framework inline scripts (RSC
// flight + hydration) run without a per-request nonce, which would break
// statically-prerendered Panel pages. The RCE class is closed natively in
// the vendored Wails dispatcher (BC3 Part A), so this CSP is
// defense-in-depth: the high-value directives (connect-src, object-src,
// base-uri, form-action, frame-ancestors) contain the residual XSS blast
// radius and need no nonce. Non-obvious allowances: cdnjs.cloudflare.com
// (jszip, loaded at runtime for client-side upload zipping); cravatar.eu +
// cdn.modrinth.com (player avatars, mod icons); frame-src stays broad so
// operator custom-tab / module iframes (already sandboxed) keep loading;
// frame-ancestors 'self' replaces the removed X-Frame-Options. connectExtra
// (the configured Panel origin, "" for same-origin /api) is the only
// cross-origin connect target - no vendor host is hardcoded.
func beamPanelCSP(connectExtra string) string {
	return "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob: https://cravatar.eu https://cdn.modrinth.com; " +
		"font-src 'self'; " +
		"connect-src 'self'" + connectExtra + "; " +
		"object-src 'none'; " +
		"base-uri 'none'; " +
		"form-action 'self'; " +
		"frame-src https: http:; " +
		"frame-ancestors 'self'"
}

// scriptNonceRe matches a CSP nonce source ('nonce-<value>'). The value charset
// covers base64 std (+ / =) and base64url (- _). The Panel emits a nonce only on
// script-src, so the first match in the header is the script-src nonce.
var scriptNonceRe = regexp.MustCompile(`'nonce-([A-Za-z0-9+/=_-]+)'`)

// extractScriptNonce returns the first CSP nonce value (without the surrounding
// 'nonce-' / quotes), or "" if the CSP carries none.
func extractScriptNonce(csp string) string {
	m := scriptNonceRe.FindStringSubmatch(csp)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// beamNonceCSP builds Beam's own Content-Security-Policy that REUSES the Panel's
// per-request nonce with 'strict-dynamic' (no 'unsafe-inline' on script-src),
// while keeping Beam's desktop-context directives: frame-src https: http: (P13
// custom-tab iframes) and frame-ancestors 'self' (Beam frames the Panel on the
// wails:// origin). Reusing Next's nonce is safe: Next stamped only its own
// legitimate inline scripts with it, and an XSS-injected inline script cannot
// predict the per-request value.
func beamNonceCSP(nonce, connectExtra string) string {
	return "default-src 'self'; " +
		"script-src 'self' 'nonce-" + nonce + "' 'strict-dynamic' https://cdnjs.cloudflare.com; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob: https://cravatar.eu https://cdn.modrinth.com; " +
		"font-src 'self'; " +
		"connect-src 'self'" + connectExtra + "; " +
		"object-src 'none'; " +
		"base-uri 'none'; " +
		"form-action 'self'; " +
		"frame-src https: http:; " +
		"frame-ancestors 'self'"
}

// beamCSPForPanel picks Beam's CSP for a proxied Panel HTML response. If the
// Panel's own CSP carried a script-src nonce, Beam reuses it under
// 'strict-dynamic' (returning the nonce so the injected runtime tags can be
// stamped to match); otherwise it falls back to the pragmatic beamPanelCSP with
// an empty nonce - byte-identical to the pre-nonce behavior, so a mixed
// old-Panel/new-Beam pair keeps working (graceful degradation).
func beamCSPForPanel(panelCSP, connectExtra string) (csp, nonce string) {
	if n := extractScriptNonce(panelCSP); n != "" {
		return beamNonceCSP(n, connectExtra), n
	}
	return beamPanelCSP(connectExtra), ""
}

// wailsRuntimeTags returns the two runtime <script> tags, stamping each with a
// nonce attribute when nonce != "" (required under 'strict-dynamic'), or the
// plain classic scripts (wailsRuntimeInjection, byte-identical to the pre-nonce
// behavior) when nonce == "".
func wailsRuntimeTags(nonce string) string {
	if nonce == "" {
		return wailsRuntimeInjection
	}
	n := ` nonce="` + nonce + `"`
	return `<script` + n + ` src="/wails/runtime.js"></script>` +
		`<script` + n + ` src="/wails/ipc.js"></script>`
}

// wailsRuntimeInjection is spliced into proxied Panel HTML so the
// window.go / window.runtime native bridge exists on the reverse-proxied
// Panel. Wails' own AssetServer only auto-injects the runtime into HTML it
// serves directly; it does NOT inject through httputil.ReverseProxy, so
// the proxied Panel needs these two tags added manually. They are plain
// classic scripts served same-origin from /wails/*, allowed by the beam
// CSP's script-src 'self'.
//
// The former inline BrowserOpenURL scheme-wrapper (and the
// Object.defineProperty freezes of window.runtime / window) were removed
// in BC3: they were a same-origin JS mitigation any page script could
// bypass via window.WailsInvoke("BO:..."). The real boundary now lives
// natively in the vendored Wails dispatcher (processBrowserMessage), which
// no page JS can reach around.
const wailsRuntimeInjection = `<script src="/wails/runtime.js"></script>` +
	`<script src="/wails/ipc.js"></script>`

// injectWailsRuntime splices the runtime scripts into an HTML document,
// preferring just before </head>, then before <body>, else prepended. The tags
// are nonce-stamped when nonce != "" (Panel supplied a nonce), else plain.
func injectWailsRuntime(html []byte, nonce string) []byte {
	tags := wailsRuntimeTags(nonce)
	s := string(html)
	for _, anchor := range []string{"</head>", "<body"} {
		if i := strings.Index(strings.ToLower(s), anchor); i >= 0 {
			return []byte(s[:i] + tags + s[i:])
		}
	}
	return append([]byte(tags), html...)
}

// beamSettingsRoute is the reserved path that serves the embedded
// Panel-URL settings page. Everything NOT under this prefix (and not
// under /wails/) is reverse-proxied to the configured Panel.
const beamSettingsRoute = "/__beam/"

// resolvePanelTarget parses the configured Panel URL into a proxy
// target. Read straight from the persisted settings on every call -
// it's a tiny local file, and keeping no cached state means a URL
// change via the Settings page takes effect on the very next request
// with zero locking.
func (a *App) resolvePanelTarget() *url.URL {
	u, err := url.Parse(strings.TrimSpace(a.GetPanelURL()))
	if err != nil || u.Scheme == "" || u.Host == "" {
		// No usable Panel URL configured. Return an empty target so the reverse
		// proxy fails into ErrorHandler, which points the user at the Settings
		// page - never a hardcoded vendor host.
		return &url.URL{}
	}
	return u
}

// newPanelMiddleware builds the AssetServer middleware that turns the
// Beam window into a transparent shell around the remote Panel.
//
// Why a proxy instead of just navigating the webview to the Panel URL:
// Wails only injects its runtime (the /wails/ipc.js + /wails/runtime.js
// scripts that create window.go / window.runtime) into HTML that the
// asset server itself serves. A cross-origin navigation to the Panel
// leaves that origin behind, so window.go is undefined there and the
// whole native bridge - gRPC file ops, native chunked upload, native
// download dialogs - silently falls back to plain HTTP. Proxying the
// Panel THROUGH the asset server keeps the webview on the wails://
// origin, so the runtime gets injected and window.go works.
//
// Routing:
//
//	/wails/*    → next (Wails runtime - must never be proxied)
//	/__beam/*   → embedded settings page (served from frontend/dist via
//	              serveEmbedded; Wails' own asset server auto-injects the
//	              runtime for the "/index.html" entry point)
//	everything  → reverse-proxied to the resolved Panel URL
func newPanelMiddleware(app *App, next http.Handler) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target := app.resolvePanelTarget()
			pr.SetURL(target)
			// SetURL leaves Out.Host as the inbound (wails.localhost)
			// host; the Panel's edge (NPM / Cloudflare) routes on Host,
			// so it has to be the real one.
			pr.Out.Host = target.Host
			// The session cookie is the shell's, not the webview's; see
			// applyShellCookies. Attached after SetURL so the jar is asked
			// about the PANEL's host rather than wails.localhost.
			applyShellCookies(pr.Out, app.panelCookies())
			// Force an uncompressed response. Wails injects its runtime
			// <script> tags into text/html bodies - it cannot do that
			// through a gzip stream. Dropping Accept-Encoding lets the
			// Go transport negotiate + transparently undo compression,
			// so the body reaching the injector is always plain HTML.
			pr.Out.Header.Del("Accept-Encoding")
		},
		ModifyResponse: func(resp *http.Response) error {
			// Take the session out of the response before anything else looks
			// at it; the webview keeps only the readable sign-in hint.
			// Read BEFORE captureShellCookies, which is what strips the header.
			// The jar can only have changed if the server sent something, and
			// every other response is an asset - a page load is hundreds of
			// them, so persisting unconditionally here put a settings-file read
			// in front of every script, stylesheet and image the panel loads.
			carriedCookies := len(resp.Header.Values("Set-Cookie")) > 0
			captureShellCookies(resp, app.panelCookies(), app.resolvePanelTarget())
			app.rememberReadableCookies(app.resolvePanelTarget(), resp)
			// Write the session through, so closing the app does not sign the
			// user out. Only on a response that carried cookies, which is the
			// sign-in, the refresh and the sign-out and nothing else.
			if carriedCookies {
				app.persistPanelSession()
			}
			// The Panel ships no security headers of its own, so beam is
			// the only place a CSP / framing policy is applied. Drop the
			// report-only variant and X-Frame-Options (the latter is
			// superseded by the CSP's frame-ancestors); the enforced
			// Content-Security-Policy is SET on proxied HTML below.
			resp.Header.Del("Content-Security-Policy-Report-Only")
			resp.Header.Del("X-Frame-Options")
			// A redirect whose Location points back at the Panel host
			// would navigate the webview off the wails:// origin and
			// kill the runtime. Rewrite it to an origin-relative path
			// so the redirect stays inside the proxy.
			if loc := resp.Header.Get("Location"); loc != "" {
				if u, err := url.Parse(loc); err == nil && u.Host != "" {
					if t := app.resolvePanelTarget(); u.Host == t.Host {
						u.Scheme, u.Host = "", ""
						resp.Header.Set("Location", u.String())
					}
				}
			}
			// Splice the Wails runtime into HTML documents so window.go
			// exists on the Panel. Accept-Encoding was stripped on the
			// way out, so the body here is always plain text.
			if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
				// Propagate the Panel's per-request nonce when present
				// (nonce-strict), else fall back to the pragmatic beamPanelCSP.
				// The Panel's own CSP is still on resp.Header here - we overwrite
				// it immediately below.
				connectExtra := beamConnectExtra(app.GetPanelURL(), app.GetAPIURL())
				csp, nonce := beamCSPForPanel(resp.Header.Get("Content-Security-Policy"), connectExtra)
				resp.Header.Set("Content-Security-Policy", csp)
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					return err
				}
				body = injectWailsRuntime(body, nonce)
				// Replay the readable cookies from the page. See
				// readableCookieScript: the Set-Cookie header above may or may
				// not reach WebView2's cookie store, and this path needs none.
				if script := app.readableCookieScript(app.resolvePanelTarget(), nonce); script != "" {
					body = injectBeforeBodyEnd(body, script)
				}
				// The only route into Beam's own settings while the panel is
				// reachable; see launcher.go.
				body = injectBeforeBodyEnd(body, launcherTag(nonce, app.updates.get()))
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.ContentLength = int64(len(body))
				resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
				resp.Header.Del("Content-Encoding")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(renderPanelUnreachable(err, app.GetPanelURL())))
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/wails/"):
			// Wails runtime + IPC bridge - always served locally.
			next.ServeHTTP(w, r)
		case r.URL.Path == "/__beam":
			// Normalize to the trailing-slash form so the settings
			// page's relative ./assets/* references resolve correctly.
			http.Redirect(w, r, beamSettingsRoute, http.StatusFound)
		case r.URL.Path == beamSettingsRoute:
			// The settings-page HTML entry point. Served through the Wails
			// asset server (which auto-injects /wails/runtime.js +
			// /wails/ipc.js for any path ending in "/index.html"), then
			// post-processed to splice the per-run shell-capability token
			// and mark the page unframeable so a compromised same-origin
			// Panel cannot iframe it and read the token. Wails' raw
			// runtime is safe now that the native dispatcher enforces the
			// URL-open boundary (BC3 Part A).
			serveBeamIndex(app, next, w, r)
		case strings.HasPrefix(r.URL.Path, beamSettingsRoute+"assets/"):
			// Static JS/CSS assets - never HTML documents, nothing to
			// inject, and Wails' own auto-injection only fires for
			// paths ending in "/" or "/index.html" so it doesn't touch
			// these either. Plain passthrough.
			serveEmbedded(next, w, r, strings.TrimPrefix(r.URL.Path, "/__beam"))
		default:
			if app.gateIsBlocked() {
				// Force-update gate active: keep the webview on the app-shell
				// mandatory screen. Redirect every Panel-bound request to
				// /__beam/ so a reload or deep-link can't slip past it.
				// /wails/ and /__beam/* are handled in the cases above, so this
				// never loops.
				http.Redirect(w, r, beamSettingsRoute, http.StatusFound)
				return
			}
			proxy.ServeHTTP(w, r)
		}
	})
}

// serveEmbedded rewrites the request path and hands it to the default
// asset handler so the embedded frontend/dist build is served.
func serveEmbedded(next http.Handler, w http.ResponseWriter, r *http.Request, path string) {
	r2 := r.Clone(r.Context())
	r2.URL.Path = path
	next.ServeHTTP(w, r2)
}

// captureResponse is a buffering http.ResponseWriter used to post-process the
// Wails asset server's /__beam/ index.html (splice the shell token, add framing
// headers) before writing it to the real client. It records status, headers,
// and body without touching the underlying connection.
type captureResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newCaptureResponse() *captureResponse {
	return &captureResponse{header: http.Header{}, status: http.StatusOK}
}

func (c *captureResponse) Header() http.Header         { return c.header }
func (c *captureResponse) WriteHeader(status int)      { c.status = status }
func (c *captureResponse) Write(b []byte) (int, error) { return c.body.Write(b) }

// injectShellToken splices a <script> that publishes the per-run shell token as
// window.__beamShellToken just before </head> (same anchor strategy as
// injectWailsRuntime). The token is emitted with %q so it is always a safe,
// Go-quoted string literal in the HTML; the token is hex from crypto/rand, so
// %q never needs to escape anything, but %q keeps it robust regardless.
func injectShellToken(html []byte, token string) []byte {
	tag := fmt.Sprintf(`<script>window.__beamShellToken=%q</script>`, token)
	s := string(html)
	for _, anchor := range []string{"</head>", "<body"} {
		if i := strings.Index(strings.ToLower(s), anchor); i >= 0 {
			return []byte(s[:i] + tag + s[i:])
		}
	}
	return append([]byte(tag), html...)
}

// serveBeamIndex serves the app-shell index.html through the Wails asset server
// (which auto-injects /wails/*), then splices the per-run shell-capability token
// into the document and marks it unframeable. The token reaches ONLY this
// first-party page, never the proxied Panel, and only on a real top-level
// navigation (see the Sec-Fetch-Dest gate below). This RAISES THE BAR:
// X-Frame-Options: DENY + frame-ancestors 'none' stop an iframe read, the
// Sec-Fetch-Dest gate stops a same-origin fetch/XHR read on webviews that send
// Fetch Metadata, and Cache-Control: no-store keeps a stale token out of caches.
// It is NOT a hard boundary on the shared wails:// origin (a webview without
// Fetch Metadata degrades to obfuscation); a true boundary needs the native
// dispatcher to check the current top-level URL at call time (deferred).
func serveBeamIndex(app *App, next http.Handler, w http.ResponseWriter, r *http.Request) {
	cap := newCaptureResponse()
	serveEmbedded(next, cap, r, "/index.html")

	body := cap.body.Bytes()
	// Splice the shell token ONLY for a genuine top-level navigation. Sec-Fetch-Dest
	// is a browser-set forbidden header that page JS cannot forge: a same-origin
	// fetch()/XHR of /__beam/ carries "empty", an iframe "iframe", a real navigation
	// "document". Without this gate a compromised proxied Panel (same wails:// origin)
	// could fetch this page and read the token from the body. If the header is ABSENT
	// (a webview with no Fetch Metadata support), we still deliver the token so the
	// shell never breaks - on such a webview this degrades to obfuscation, no worse
	// than not gating at all.
	dest := r.Header.Get("Sec-Fetch-Dest")
	isNavigation := dest == "" || dest == "document"
	if isNavigation && strings.HasPrefix(cap.header.Get("Content-Type"), "text/html") {
		body = injectShellToken(body, app.shellToken)
	}

	// Copy the asset server's headers through, then override framing + length.
	for k, vals := range cap.header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	// no-store: the shell token is per-run, so never serve a stale cached copy
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Del("Content-Encoding")

	status := cap.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// renderPanelUnreachable fills the {{ERROR}} slot of panelUnreachableHTML with
// the transport error, HTML-ESCAPED.
//
// The error string is not ours. A Go transport error can carry bytes the far
// end chose - an HTTP/2 GoAwayError renders the server's debug data verbatim -
// so the Panel we just failed to reach can put markup in here. This page is
// served on the wails:// origin, the one where window.go is bound and where the
// rest of this file's CSP and framing work exists to contain a compromised
// Panel; handing that same Panel a script tag through the error path would walk
// straight around all of it. Extracted from the ErrorHandler closure so the
// invariant is unit-testable, the same way isBrowserOpenableURL was.
func renderPanelUnreachable(err error, panelURL string) string {
	page := strings.ReplaceAll(panelUnreachableHTML, "{{ERROR}}", html.EscapeString(err.Error()))
	return strings.ReplaceAll(page, "{{PANEL}}", html.EscapeString(panelURL))
}

// panelUnreachableHTML is what the window shows when the Panel cannot be
// reached - DNS failure, connection refused, TLS error, a panel that is simply
// restarting.
//
// It RETRIES on its own, and that is the point of it. The previous version was
// a dead end: one sentence and a link to the settings page, so a panel that was
// down for thirty seconds left the user staring at an error inviting them to
// change a URL that was correct. Backing off from 3s to 30s means an app left
// open comes back by itself when the panel does.
//
// The settings link stays, and is now the app's only always-available way in:
// when the panel IS reachable the window is the panel, with nothing of Beam's
// own on screen.
//
// Everything here is inline and dependency-free on purpose. This page is what
// is left when the network is gone, so it cannot fetch a stylesheet, a font or
// a script. {{ERROR}} and {{PANEL}} are substituted HTML-escaped; see
// renderPanelUnreachable for why that matters.
const panelUnreachableHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Dylaris Beam - Panel unreachable</title>
<style>
  html,body{height:100%;margin:0}
  body{display:flex;align-items:center;justify-content:center;
       background:#080909;color:#D8DDE6;
       font-family:system-ui,-apple-system,sans-serif}
  .box{max-width:520px;padding:32px;text-align:center}
  h1{font-size:18px;font-weight:600;margin:0 0 8px}
  p{font-size:13px;color:#8A95A8;line-height:1.6;margin:0 0 16px}
  .target{font-family:ui-monospace,monospace;font-size:12px;color:#D8DDE6}
  code{display:block;font-family:ui-monospace,monospace;font-size:11px;
       color:#5A6272;word-break:break-all;margin:0 0 20px}
  .status{font-size:12px;color:#8A95A8;margin:0 0 20px;min-height:18px}
  .dot{display:inline-block;width:7px;height:7px;border-radius:50%;
       background:#7048C8;margin-right:7px;vertical-align:middle}
  @keyframes pulse{0%,100%{opacity:1}50%{opacity:.25}}
  .dot.live{animation:pulse 1.2s ease-in-out infinite}
  @media (prefers-reduced-motion:reduce){.dot.live{animation:none}}
  .row{display:flex;gap:8px;justify-content:center}
  button,a{font:inherit;font-size:13px;font-weight:500;padding:9px 16px;
    border-radius:8px;border:1px solid transparent;cursor:pointer;
    text-decoration:none;display:inline-block}
  .primary{background:#7048C8;color:#fff}
  .primary:hover{background:#7E58D4}
  .primary:disabled{opacity:.5;cursor:default}
  .secondary{background:transparent;color:#D8DDE6;border-color:#2A2F38}
  .secondary:hover{border-color:#3A414D;background:#12141A}
  button:focus-visible,a:focus-visible{outline:2px solid #7048C8;outline-offset:2px}
</style>
</head>
<body>
  <div class="box">
    <h1>Can&#39;t reach the Panel</h1>
    <p>Beam could not connect to <span class="target">{{PANEL}}</span>.</p>
    <code>{{ERROR}}</code>
    <p class="status" id="status" role="status" aria-live="polite"></p>
    <div class="row">
      <button type="button" class="primary" id="retry">Try again</button>
      <a class="secondary" href="/__beam/">Settings</a>
    </div>
  </div>
<script>
(function () {
  var status = document.getElementById('status');
  var retry = document.getElementById('retry');
  var attempt = 0;
  var timer = null;

  // 3s, then doubling to a 30s ceiling. A panel that is restarting is back
  // within the first few tries; one that is down for the evening must not have
  // this window hammering it once a second.
  function delayFor(n) { return Math.min(30000, 3000 * Math.pow(2, n)); }

  function show(text, live) {
    status.innerHTML = '<span class="dot' + (live ? ' live' : '') + '"></span>' + text;
  }

  function check() {
    show('Reconnecting...', true);
    retry.disabled = true;
    // A HEAD through the proxy reaches the panel exactly the way the page
    // itself would; cache:no-store so a cached 502 cannot answer for it.
    fetch('/', { method: 'HEAD', cache: 'no-store' })
      .then(function (res) {
        if (res.ok) { location.replace('/'); return; }
        schedule();
      })
      .catch(schedule);
  }

  function schedule() {
    retry.disabled = false;
    var wait = delayFor(attempt);
    attempt++;
    var left = Math.round(wait / 1000);
    show('Attempt ' + attempt + ' failed. Retrying in ' + left + 's.', false);
    clearInterval(timer);
    timer = setInterval(function () {
      left--;
      if (left <= 0) { clearInterval(timer); check(); return; }
      show('Attempt ' + attempt + ' failed. Retrying in ' + left + 's.', false);
    }, 1000);
  }

  retry.addEventListener('click', function () { clearInterval(timer); attempt = 0; check(); });
  schedule();
})();
</script>
</body>
</html>`

// The session lives in the SHELL, not in the webview.
//
// Core moved the panel session into an HttpOnly cookie when it started serving
// the panel itself, and that is right for a browser. Beam is not one. Its window
// sits on http://wails.localhost and every response is handed to WebView2 as a
// CUSTOM response to an intercepted request - a path whose Set-Cookie handling
// is not something the WebView2 API documents, and whose origin is plain http
// while the cookie Core sends behind TLS is marked Secure. Depending on that
// working is depending on an undocumented behaviour of an embedded browser.
//
// So the proxy keeps the credential on the Go side and attaches it to every
// upstream request. That removes the dependency, and it is strictly better than
// the alternative: HttpOnly means "the page must not hold this", and here the
// page genuinely never does.
//
// The READABLE companion cookie still goes to the webview, because the panel
// answers "am I signed in?" from it before its first API call. It is stripped of
// Secure on the way, since the origin it is being stored against is http.
//
// ponytail: the jar is in-memory, so closing Beam ends the session. Persist it
// to the settings file if staying signed in across restarts is worth putting a
// live JWT on disk.

// applyShellCookies puts the shell's cookies on an outbound proxied request,
// replacing any same-named cookie the webview sent so the jar is the authority.
func applyShellCookies(out *http.Request, jar http.CookieJar) {
	held := jar.Cookies(out.URL)
	if len(held) == 0 {
		return
	}
	owned := make(map[string]bool, len(held))
	for _, c := range held {
		owned[c.Name] = true
	}
	merged := make([]*http.Cookie, 0, len(held))
	for _, c := range out.Cookies() {
		if !owned[c.Name] {
			merged = append(merged, c)
		}
	}
	merged = append(merged, held...)
	out.Header.Del("Cookie")
	for _, c := range merged {
		out.AddCookie(c)
	}
}

// captureShellCookies moves the upstream's cookies into the jar and rewrites
// what is left for the webview.
//
// It returns early on a response that set none, so the ordinary case never has
// its headers rebuilt - a proxy that reconstructs a header it did not have to
// touch is a proxy that eventually drops something it did not understand.
func captureShellCookies(resp *http.Response, jar http.CookieJar, target *url.URL) {
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		return
	}
	jar.SetCookies(target, cookies)

	resp.Header.Del("Set-Cookie")
	for _, c := range cookies {
		if c.HttpOnly {
			// The credential. The shell holds it; the webview never sees it.
			continue
		}
		// The webview origin is http://wails.localhost. Secure would be a
		// request to store this over a transport it is not on, and Domain names
		// the panel's host, which is not the host this response arrives on.
		c.Secure = false
		c.Domain = ""
		if v := c.String(); v != "" {
			resp.Header.Add("Set-Cookie", v)
		}
	}
}

// rememberReadableCookies keeps the cookies the webview is allowed to hold, so
// they can be replayed into the page.
//
// Call it AFTER captureShellCookies, which is what has already stripped the
// HttpOnly ones and corrected Secure/Domain for the wails.localhost origin -
// what is left on the response is exactly what the webview should end up with.
//
// Deletions are kept rather than dropped: a "Max-Age=0" line is the instruction
// that clears a stale copy, and forgetting it would leave the page believing it
// is signed in after a logout.
func (a *App) rememberReadableCookies(target *url.URL, resp *http.Response) {
	values := resp.Header.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}
	key := panelKey(target)
	a.readableMu.Lock()
	defer a.readableMu.Unlock()
	if a.readable == nil {
		a.readable = map[string]map[string]string{}
	}
	if a.readable[key] == nil {
		a.readable[key] = map[string]string{}
	}
	for _, v := range values {
		name, _, ok := strings.Cut(v, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		a.readable[key][name] = v
		// Core has spoken about this cookie again, so a pending local deletion
		// is stale - it would otherwise delete the sign-in the user just made.
		delete(a.readableGone, name)
	}
}

// readableCookieScript returns an inline script that writes the readable
// cookies into document.cookie, or "" when there are none.
//
// It exists because Beam's responses reach WebView2 as CUSTOM responses to
// intercepted requests, and whether that path feeds the browser's cookie store
// is not documented. Setting them from the page needs no cookie store at all,
// and is idempotent if the header worked anyway.
//
// The values are JSON-encoded rather than pasted in. They are cookie values
// Core chose, but this writes them into a <script> element, and the way that
// bites is a value containing "</script>" - so the encoding covers the tag, not
// only the quotes.
func (a *App) readableCookieScript(target *url.URL, nonce string) string {
	a.readableMu.Lock()
	defer a.readableMu.Unlock()
	held := a.readable[panelKey(target)]
	if len(held) == 0 && len(a.readableGone) == 0 {
		return ""
	}
	lines := make([]string, 0, len(held)+len(a.readableGone))
	emit := func(v string) {
		enc, err := json.Marshal(v)
		if err != nil {
			return
		}
		// json.Marshal escapes <, > and & by default, which is what keeps a
		// value containing "</script>" from closing this element.
		lines = append(lines, "document.cookie="+string(enc)+";")
	}
	for _, v := range held {
		emit(v)
	}
	// Deletions for cookies dropped locally, whatever panel this page belongs
	// to: every panel is proxied onto one origin, so that is where the copy is.
	// A name Core has set again is no longer in here (rememberReadableCookies).
	for name, v := range a.readableGone {
		if _, stillHeld := held[name]; stillHeld {
			continue
		}
		emit(v)
	}
	sort.Strings(lines) // stable output, so the injected bytes do not churn
	attr := ""
	if nonce != "" {
		attr = ` nonce="` + nonce + `"`
	}
	return "<script" + attr + ">" + strings.Join(lines, "") + "</script>"
}

// injectBeforeBodyEnd splices markup in just before </body>, falling back to
// the end of the document.
//
// The cookie replay has to run BEFORE the panel's own scripts read
// document.cookie, but it must not sit in <head> where the Wails runtime tags
// are: those are what the panel's bundle waits for, and a document.cookie write
// wedged between them is a change to a load order that has been working.
// Before </body> is after the runtime and before the bundle executes.
func injectBeforeBodyEnd(doc []byte, markup string) []byte {
	s := string(doc)
	if i := strings.LastIndex(strings.ToLower(s), "</body>"); i >= 0 {
		return []byte(s[:i] + markup + s[i:])
	}
	return append(doc, markup...)
}
