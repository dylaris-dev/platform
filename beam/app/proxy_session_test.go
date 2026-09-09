package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// The session the panel hands out is an HttpOnly cookie, and Beam is the one
// client that is not an ordinary browser: the window sits on http://wails.localhost
// and every response is handed to WebView2 as a CUSTOM response from an
// intercepted request, not as something its network stack fetched.
//
// So the shell holds the session itself. Two properties make that work, and
// both have failed on their own:
//
//   - The HttpOnly cookie must never reach the webview. It is the credential,
//     and the whole point of HttpOnly is that the page cannot hold it.
//   - The READABLE companion must reach it, without Secure. The panel decides
//     "am I signed in?" from that cookie before its first API call; if it does
//     not arrive, the user logs in successfully and is bounced straight back to
//     the login screen, which looks exactly like the page reloading.
func TestShellHoldsTheSessionCookie(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("https://panel.example.com/")

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "dylaris_session=secret-jwt; Path=/; Max-Age=86400; HttpOnly; Secure; SameSite=Lax")
	resp.Header.Add("Set-Cookie", "dylaris_signed_in=1; Path=/; Max-Age=86400; Secure; SameSite=Lax")

	captureShellCookies(resp, jar, target)

	forwarded := resp.Header.Values("Set-Cookie")
	joined := strings.Join(forwarded, "\n")
	if strings.Contains(joined, "secret-jwt") {
		t.Error("the HttpOnly session reached the webview; the shell must keep it")
	}
	if !strings.Contains(joined, "dylaris_signed_in=1") {
		t.Fatalf("the readable sign-in hint was dropped, so the panel bounces back to login: %q", joined)
	}
	if strings.Contains(joined, "Secure") {
		t.Errorf("Secure survived onto an http://wails.localhost response: %q", joined)
	}

	// ...and the credential the shell kept has to go back out on the next
	// request, or nothing is authenticated at all.
	out := httptest.NewRequest(http.MethodGet, "https://panel.example.com/api/me", nil)
	out.AddCookie(&http.Cookie{Name: "dylaris_signed_in", Value: "1"})
	applyShellCookies(out, jar)
	got := map[string]string{}
	for _, c := range out.Cookies() {
		got[c.Name] = c.Value
	}
	if got["dylaris_session"] != "secret-jwt" {
		t.Errorf("the session was not attached to the upstream request: %v", got)
	}
	if got["dylaris_signed_in"] != "1" {
		t.Errorf("an unrelated browser cookie was dropped: %v", got)
	}
}

// Logging out has to clear the shell's copy too. The browser never held the
// session, so Core's delete instruction reaches nothing unless the jar applies
// it - and a shell that kept sending the old cookie would leave the user signed
// in on a window that shows the login form.
func TestLogoutClearsTheShellCopy(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	target, _ := url.Parse("https://panel.example.com/")

	set := &http.Response{Header: http.Header{}}
	set.Header.Add("Set-Cookie", "dylaris_session=secret-jwt; Path=/; Max-Age=86400; HttpOnly")
	captureShellCookies(set, jar, target)

	clear := &http.Response{Header: http.Header{}}
	clear.Header.Add("Set-Cookie", "dylaris_session=; Path=/; Max-Age=0; HttpOnly")
	captureShellCookies(clear, jar, target)

	out := httptest.NewRequest(http.MethodGet, "https://panel.example.com/api/me", nil)
	applyShellCookies(out, jar)
	for _, c := range out.Cookies() {
		if c.Name == "dylaris_session" && c.Value != "" {
			t.Errorf("the shell still holds a session after logout: %q", c.Value)
		}
	}
}

// A response with no cookies must come out byte-identical. Rebuilding the
// header unconditionally is how a proxy quietly drops something it did not
// understand.
func TestAResponseWithoutCookiesIsUntouched(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	target, _ := url.Parse("https://panel.example.com/")
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"text/html"}}}
	captureShellCookies(resp, jar, target)
	if _, ok := resp.Header["Set-Cookie"]; ok {
		t.Error("a Set-Cookie header appeared on a response that had none")
	}
	if resp.Header.Get("Content-Type") != "text/html" {
		t.Error("an unrelated header was disturbed")
	}
}

// Forwarding the readable cookie in a Set-Cookie header is only half of it.
//
// Beam's responses reach WebView2 as CUSTOM responses to intercepted requests,
// and whether that path feeds the cookie store is not something the WebView2 API
// documents. If it does not, the panel never sees the sign-in hint, decides it
// is signed out, and bounces a successfully-authenticated user back to the login
// screen - which is exactly what "it just reloads on login" looks like.
//
// So the shell also writes those cookies from the page itself, where no network
// cookie store is involved. Idempotent: if the header worked, this sets the same
// value again.
func TestTheReadableCookiesAreAlsoWrittenFromThePage(t *testing.T) {
	a := &App{}
	target, _ := url.Parse("https://panel.example.com/")

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "dylaris_session=secret-jwt; Path=/; HttpOnly; Secure")
	resp.Header.Add("Set-Cookie", "dylaris_signed_in=1; Path=/; Max-Age=86400; Secure")
	captureShellCookies(resp, a.panelCookies(), target)
	a.rememberReadableCookies(target, resp)

	script := a.readableCookieScript(target, "nonce123")
	if !strings.Contains(script, "dylaris_signed_in=1") {
		t.Errorf("the sign-in hint is not replayed into the page: %q", script)
	}
	if strings.Contains(script, "secret-jwt") {
		t.Fatal("the HttpOnly session was written into document.cookie")
	}
	if !strings.Contains(script, `nonce="nonce123"`) {
		t.Errorf("the script is not nonce-stamped, so a strict CSP drops it: %q", script)
	}
	// A quote in a cookie value must not be able to close the script literal.
	a2 := &App{}
	bad := &http.Response{Header: http.Header{}}
	bad.Header.Add("Set-Cookie", `x=</script><script>alert(1)</script>; Path=/`)
	a2.rememberReadableCookies(target, bad)
	if strings.Contains(a2.readableCookieScript(target, ""), "<script>alert(1)") {
		t.Error("a cookie value broke out of the injected script")
	}
}

// Nothing to replay must inject nothing at all. An empty <script> on every HTML
// response is noise that a strict CSP then has to be widened for.
func TestNoSessionInjectsNoScript(t *testing.T) {
	a := &App{}
	target, _ := url.Parse("https://panel.example.com/")
	if got := a.readableCookieScript(target, "n"); got != "" {
		t.Errorf("a script was injected with no cookies held: %q", got)
	}
}

// Persisting the session may not ride on every proxied response.
//
// ModifyResponse runs for every asset, and a panel page is hundreds of them. A
// settings read per response put file IO in front of every script, stylesheet
// and image the panel loads. The jar can only change when the server actually
// sent cookies, so that is the condition.
func TestSessionIsPersistedOnlyWhenCookiesArrive(t *testing.T) {
	b, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, "carriedCookies := len(resp.Header.Values(\"Set-Cookie\")) > 0") {
		t.Error("nothing records whether the response carried cookies")
	}
	if !strings.Contains(src, "if carriedCookies {\n\t\t\t\tapp.persistPanelSession()") {
		t.Error("persistPanelSession is not gated on the response carrying cookies")
	}
	// It has to be read BEFORE captureShellCookies, which deletes the header.
	if strings.Index(src, "carriedCookies :=") > strings.Index(src, "captureShellCookies(resp,") {
		t.Error("the header is read after it has been stripped, so the flag is always false")
	}
}

// "Clear local data" has to clear BOTH records of the session, and used to
// clear one.
//
// The shell's jar is what authenticates a proxied request; the readable set is
// what the injected script writes into document.cookie so the panel can tell it
// is signed in. Dropping only the jar leaves the next page load re-injecting a
// sign-in hint for a session that no longer exists - the panel renders its
// authed shell and every request inside it fails, which is worse than being
// signed out and is not what the user asked for.
func TestClearingLocalDataForgetsTheReadableCookiesToo(t *testing.T) {
	a := &App{}
	target, _ := url.Parse("https://panel.example.com/")

	// Somewhere else for the session file, so a real one is never touched.
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "dylaris_session=secret-jwt; Path=/; HttpOnly; Secure")
	resp.Header.Add("Set-Cookie", "dylaris_signed_in=1; Path=/; Max-Age=86400; Secure")
	captureShellCookies(resp, a.panelCookies(), target)
	a.rememberReadableCookies(target, resp)

	if a.readableCookieScript(target, "") == "" {
		t.Fatal("nothing was remembered, so this proves nothing about clearing it")
	}

	a.forgetPanelSession()

	if got := a.readableCookieScript(target, ""); got != "" {
		t.Errorf("the sign-in hint survived the clear and will be replayed: %q", got)
	}
}
