package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Keeping the panel session across restarts.
//
// The shell holds the panel session in its OWN cookie jar rather than letting
// the webview store it (see applyShellCookies). That jar was built with
// cookiejar.New and never written anywhere, so it died with the process: every
// launch of Beam landed on the login form, no matter how fresh the session
// still was on the server.
//
// It is deliberately NOT in config.json. That file is written 0644 because it
// holds panel URLs and nothing else; a session token in it would be readable by
// every other account on the machine. This one is 0600 and separate, so the
// permission tells the truth about what the file contains.
//
// Only name and value survive a round trip - http.CookieJar can hand back the
// cookies that apply to a URL, not their attributes. That is exactly what
// applyShellCookies puts on the wire anyway, so nothing is lost: the server's
// own expiry still decides when the session ends, and a stale entry is refused
// once and replaced by a fresh login.

// storedCookie is one cookie as it can be recovered from a jar.
type storedCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// storedSessions is the file's schema, keyed by panel ORIGIN. Per origin
// because the app holds several panels and a session belongs to exactly one of
// them; a single blob would send a production cookie to a test panel on the
// first switch.
type storedSessions struct {
	Cookies map[string][]storedCookie `json:"cookies"`
}

// sessionPath is in the user's LOCAL data directory (%LocalAppData% on
// Windows), not beside config.json in the roaming one: a roaming profile
// copies %AppData% to every machine the user signs in to, and a live session
// token has no business travelling with it. On Windows the file is also
// encrypted to the user account (sealSession).
func sessionPath() string {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return legacySessionPath()
	}
	return filepath.Join(dir, "dylaris-beam", "session.json")
}

// legacySessionPath is where earlier versions wrote the session, in plain
// JSON beside config.json. Read once, moved, and removed.
func legacySessionPath() string {
	return filepath.Join(filepath.Dir(settingsPath()), "session.json")
}

// sealedMagic marks a session file encrypted with sealSession.
var sealedMagic = []byte("DYLBEAM-SEALED-1\n")

// panelOrigin reduces a panel URL to the scheme://host key used in the file.
// Path and query are dropped: cookies are scoped by host, and keying on the
// full URL would store one session per page the user happened to be on.
func panelOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Scheme == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func readStoredSessions() storedSessions {
	var s storedSessions
	data, err := os.ReadFile(sessionPath())
	if err != nil {
		// A session from an earlier version: take it over, store it the new
		// way, and leave no plaintext copy behind.
		legacy := legacySessionPath()
		if legacy == sessionPath() {
			return s
		}
		old, lerr := os.ReadFile(legacy)
		if lerr != nil {
			return s
		}
		if json.Unmarshal(old, &s) == nil && writeStoredSessions(s) == nil {
			_ = os.Remove(legacy)
		}
		return s
	}
	if bytes.HasPrefix(data, sealedMagic) {
		plain, err := openSession(data[len(sealedMagic):])
		if err != nil {
			return storedSessions{} // another account's file, or a copied one: sign in again
		}
		data = plain
	}
	_ = json.Unmarshal(data, &s)
	return s
}

func writeStoredSessions(s storedSessions) error {
	path := sessionPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if sessionSealed {
		sealed, err := sealSession(data)
		if err != nil {
			return err
		}
		data = append(append([]byte(nil), sealedMagic...), sealed...)
	}
	// 0600: this file holds a live session token. On Windows the mode is
	// advisory, which is why it is sealed there as well.
	return os.WriteFile(path, data, 0o600)
}

// restoreInto pushes every stored cookie back into a fresh jar.
//
// An unparseable origin is skipped rather than failing the whole restore: one
// bad entry from a hand-edited file must not cost the user every other session.
func restoreInto(jar http.CookieJar, s storedSessions) {
	for origin, cookies := range s.Cookies {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			continue
		}
		out := make([]*http.Cookie, 0, len(cookies))
		for _, c := range cookies {
			if c.Name == "" {
				continue
			}
			out = append(out, &http.Cookie{Name: c.Name, Value: c.Value, Path: "/"})
		}
		if len(out) > 0 {
			jar.SetCookies(u, out)
		}
	}
}

// collectFrom reads back what the jar currently holds for the given origins.
//
// Sorted, because the result is compared against the last write to decide
// whether to touch the disk at all, and Go's map iteration order would make an
// unchanged session look different on every response.
func collectFrom(jar http.CookieJar, origins []string) storedSessions {
	out := storedSessions{Cookies: map[string][]storedCookie{}}
	for _, origin := range origins {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			continue
		}
		got := jar.Cookies(u)
		if len(got) == 0 {
			continue
		}
		list := make([]storedCookie, 0, len(got))
		for _, c := range got {
			list = append(list, storedCookie{Name: c.Name, Value: c.Value})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		out.Cookies[origin] = list
	}
	return out
}

// fingerprint is the cheap "did anything change" key. Cookies are captured on
// EVERY proxied response, and rewriting the file each time would be a disk
// write per navigation for a value that changes at login and logout.
func (s storedSessions) fingerprint() string {
	origins := make([]string, 0, len(s.Cookies))
	for o := range s.Cookies {
		origins = append(origins, o)
	}
	sort.Strings(origins)
	var b strings.Builder
	for _, o := range origins {
		b.WriteString(o)
		b.WriteByte('|')
		for _, c := range s.Cookies[o] {
			b.WriteString(c.Name)
			b.WriteByte('=')
			b.WriteString(c.Value)
			b.WriteByte(';')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// knownOrigins is every panel this install could hold a session for: the saved
// list plus whatever is active. Read from settings rather than remembered on
// the App, so a panel added during this run is covered without a restart.
func knownOrigins() []string {
	s := loadSettings()
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		if o := panelOrigin(raw); o != "" && !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	for _, p := range s.panelList() {
		add(p.URL)
	}
	add(s.activePanel().URL)
	return out
}

// persistPanelSession writes the jar out when it has changed. Called after
// every proxied response, which is why the no-change path does no IO.
func (a *App) persistPanelSession() {
	a.cookieMu.Lock()
	jar := a.cookies
	a.cookieMu.Unlock()
	if jar == nil {
		return
	}
	current := collectFrom(jar, knownOrigins())
	fp := current.fingerprint()

	a.sessionMu.Lock()
	unchanged := fp == a.sessionFingerprint
	if !unchanged {
		a.sessionFingerprint = fp
	}
	a.sessionMu.Unlock()
	if unchanged {
		return
	}
	_ = writeStoredSessions(current)
}

// clearStoredSession removes the file. Paired with forgetPanelSession: dropping
// the in-memory jar while leaving the token on disk would put the user back
// where they were on the next launch, which is the opposite of signing out.
func (a *App) clearStoredSession() {
	a.sessionMu.Lock()
	a.sessionFingerprint = ""
	a.sessionMu.Unlock()
	_ = os.Remove(sessionPath())
	_ = os.Remove(legacySessionPath())
}
