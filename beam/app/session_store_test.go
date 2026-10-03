package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
)

func newJar(t *testing.T) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return jar
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The behaviour the user reported: Beam asked for a login on every launch,
// because the shell's cookie jar was built with cookiejar.New and never written
// anywhere. A session that survives a restart is the whole point of this file.
func TestSessionSurvivesAFreshJar(t *testing.T) {
	origin := "https://panel.example.com"
	u := mustURL(t, origin+"/servers")

	first := newJar(t)
	first.SetCookies(u, []*http.Cookie{{Name: "dylaris_session", Value: "abc123", Path: "/"}})

	saved := collectFrom(first, []string{origin})

	// A brand new process: nothing in memory, everything from the file.
	second := newJar(t)
	if got := second.Cookies(u); len(got) != 0 {
		t.Fatalf("a fresh jar already had %d cookies", len(got))
	}
	restoreInto(second, saved)

	got := second.Cookies(u)
	if len(got) != 1 || got[0].Name != "dylaris_session" || got[0].Value != "abc123" {
		t.Errorf("restored %v, want the session cookie - the user is back on the login form", got)
	}
}

// A session belongs to ONE panel. The app holds several, so a store keyed by
// anything coarser would send the production session to a test panel the first
// time someone switches.
func TestSessionIsPerPanel(t *testing.T) {
	a, b := "https://panel-a.example.com", "https://panel-b.example.com"
	jar := newJar(t)
	jar.SetCookies(mustURL(t, a), []*http.Cookie{{Name: "s", Value: "from-a", Path: "/"}})
	jar.SetCookies(mustURL(t, b), []*http.Cookie{{Name: "s", Value: "from-b", Path: "/"}})

	saved := collectFrom(jar, []string{a, b})
	restored := newJar(t)
	restoreInto(restored, saved)

	if got := restored.Cookies(mustURL(t, a)); len(got) != 1 || got[0].Value != "from-a" {
		t.Errorf("panel A restored %v, want from-a", got)
	}
	if got := restored.Cookies(mustURL(t, b)); len(got) != 1 || got[0].Value != "from-b" {
		t.Errorf("panel B restored %v, want from-b", got)
	}
}

// A panel this install does not know about must not come back. The file is
// keyed by origin and read wholesale, so without this a stale entry for a panel
// the user removed would keep re-entering the jar.
func TestCollectOnlyReadsTheOriginsAsked(t *testing.T) {
	jar := newJar(t)
	jar.SetCookies(mustURL(t, "https://kept.example.com"), []*http.Cookie{{Name: "s", Value: "1", Path: "/"}})
	jar.SetCookies(mustURL(t, "https://dropped.example.com"), []*http.Cookie{{Name: "s", Value: "2", Path: "/"}})

	saved := collectFrom(jar, []string{"https://kept.example.com"})
	if _, ok := saved.Cookies["https://dropped.example.com"]; ok {
		t.Error("an origin that was not asked for was collected anyway")
	}
	if len(saved.Cookies["https://kept.example.com"]) != 1 {
		t.Error("the asked-for origin was not collected")
	}
}

// The fingerprint decides whether the disk is touched at all, and it runs on
// EVERY proxied response. Map iteration order must not make an unchanged
// session look different, or the file is rewritten on every navigation.
func TestFingerprintIsStableAndNoticesChange(t *testing.T) {
	build := func(value string) storedSessions {
		return storedSessions{Cookies: map[string][]storedCookie{
			"https://a.example.com": {{Name: "s", Value: value}},
			"https://b.example.com": {{Name: "s", Value: "other"}},
		}}
	}
	// Two separately built maps, not the same expression twice: what is being
	// tested is that Go's randomised map iteration order does not reach the
	// output.
	first := build("x").fingerprint()
	second := build("x").fingerprint()
	if first != second {
		t.Error("the same session fingerprinted differently - every response would rewrite the file")
	}
	if changed := build("y").fingerprint(); first == changed {
		t.Error("a changed session fingerprinted the same - a fresh login would never be saved")
	}
}

// Signing out has to reach the disk. Dropping only the in-memory jar would put
// the user straight back into the account they just left, on the next launch.
func TestClearStoredSessionRemovesTheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // linux
	t.Setenv("AppData", dir)         // windows
	t.Setenv("LocalAppData", dir)    // Windows: the session file
	t.Setenv("XDG_CACHE_HOME", dir)  // Linux: the session file
	t.Setenv("HOME", dir)            // macOS falls back through here
	if err := writeStoredSessions(storedSessions{Cookies: map[string][]storedCookie{
		"https://panel.example.com": {{Name: "s", Value: "v"}},
	}}); err != nil {
		t.Fatal(err)
	}
	path := sessionPath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the session file was not written: %v", err)
	}
	(&App{}).clearStoredSession()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the session file survived a sign-out: %v", err)
	}
}

// The token must not land in config.json. That file is 0644 because it holds
// panel URLs; a session in it would be readable by every other account on the
// machine.
func TestSessionIsNotStoredBesideThePanelURLs(t *testing.T) {
	if sessionPath() == settingsPath() {
		t.Fatal("the session is written into the settings file, which is world-readable")
	}
	b, err := os.ReadFile("session_store.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "0o600") {
		t.Error("the session file is not written 0600")
	}
}

func TestPanelOriginDropsPathAndRejectsJunk(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://panel.example.com/servers/42?x=1", "https://panel.example.com"},
		{"  https://panel.example.com  ", "https://panel.example.com"},
		{"http://localhost:25510/", "http://localhost:25510"},
		{"panel.example.com", ""}, // no scheme: not an origin
		{"", ""},
	} {
		if got := panelOrigin(c.in); got != c.want {
			t.Errorf("panelOrigin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
