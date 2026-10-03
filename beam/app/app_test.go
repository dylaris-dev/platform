package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestNewAppMintsShellToken(t *testing.T) {
	a := NewApp()
	// 32 random bytes hex-encoded = 64 hex chars.
	if len(a.shellToken) != 64 {
		t.Fatalf("shellToken len = %d, want 64", len(a.shellToken))
	}
	if _, err := hex.DecodeString(a.shellToken); err != nil {
		t.Errorf("shellToken is not hex: %v", err)
	}
	// Two independent mints must differ (proves it is random, not fixed).
	if b := NewApp(); a.shellToken == b.shellToken {
		t.Error("two NewApp() calls produced the same shellToken - not random")
	}
}

func TestCheckShellToken(t *testing.T) {
	a := &App{shellToken: "abcdef0123456789"}
	cases := []struct {
		name string
		tok  string
		want bool
	}{
		{"exact match", "abcdef0123456789", true},
		{"mismatch same length", "0000000000000000", false},
		{"empty", "", false},
		{"prefix only", "abcdef", false},
		{"longer", "abcdef0123456789x", false},
	}
	for _, c := range cases {
		if got := a.checkShellToken(c.tok); got != c.want {
			t.Errorf("%s: checkShellToken(%q) = %v, want %v", c.name, c.tok, got, c.want)
		}
	}
}

func TestSavePanelURLTokenGate(t *testing.T) {
	// Redirect the settings path into a temp dir so the happy-path write does
	// not touch the real user config. os.UserConfigDir reads AppData on
	// Windows and XDG_CONFIG_HOME / HOME on Unix, so set all three.
	tmp := t.TempDir()
	t.Setenv("AppData", tmp)         // Windows
	t.Setenv("LocalAppData", tmp)    // Windows: the session file
	t.Setenv("XDG_CACHE_HOME", tmp)  // Linux: the session file
	t.Setenv("XDG_CONFIG_HOME", tmp) // Linux
	t.Setenv("HOME", tmp)            // macOS / Linux fallback

	a := &App{shellToken: "good-token"}

	// Bad token: rejected as "unauthorized" BEFORE any disk write.
	if err := a.SavePanelURL("wrong-token", "https://evil.test"); err == nil {
		t.Fatal("SavePanelURL accepted a bad token - must reject")
	}
	if _, err := os.Stat(settingsPath()); err == nil {
		t.Error("SavePanelURL wrote config despite a bad token")
	}

	// Good token: accepted, writes into the temp dir.
	if err := a.SavePanelURL("good-token", "https://panel.example.test"); err != nil {
		t.Fatalf("SavePanelURL rejected a good token: %v", err)
	}
	// Read through activePanel rather than off the legacy field: the setting
	// moved into a LIST when the app learned about several panels, and the
	// legacy field is now only a migration source. Asserting the old field would
	// be asserting the storage shape rather than what the app resolves.
	if got := loadSettings().activePanel().URL; got != "https://panel.example.test" {
		t.Errorf("active panel = %q, want https://panel.example.test", got)
	}
}

// panelOriginTestApp returns an App whose Panel target is fixed to
// https://panel.example.test, with the settings path redirected to an empty temp
// dir so loadSettings() finds no saved override and GetPanelURL falls through to
// a.panelURL. Keeps the origin check deterministic and off the real user config.
func panelOriginTestApp(t *testing.T) *App {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("AppData", tmp)         // Windows
	t.Setenv("LocalAppData", tmp)    // Windows: the session file
	t.Setenv("XDG_CACHE_HOME", tmp)  // Linux: the session file
	t.Setenv("XDG_CONFIG_HOME", tmp) // Linux
	t.Setenv("HOME", tmp)            // macOS / Linux fallback
	// Launching with DYLARIS_PANEL_URL set is what this simulates, and that now
	// moves the built-in list entry too - otherwise panelList would offer
	// panel.dylaris.com and activePanel would resolve to it, so an app "started"
	// against a test panel would report the production one as its origin.
	withBuiltInPanel(t, "https://panel.example.test", "")
	return &App{panelURL: "https://panel.example.test"}
}

// withBuiltInPanel points the built-in entry somewhere else for one test and
// puts it back afterwards. Package-level state, so restoring it is not optional:
// a leaked value would make every later test in the binary see a panel it never
// configured.
func withBuiltInPanel(t *testing.T, panelURL, apiURL string) {
	t.Helper()
	oldPanel, oldAPI := builtInPanelURL, builtInAPIURL
	setBuiltInDefaults(panelURL, apiURL)
	t.Cleanup(func() { setBuiltInDefaults(oldPanel, oldAPI) })
}

func TestIsPanelOrigin(t *testing.T) {
	a := panelOriginTestApp(t)
	cases := []struct {
		name   string
		apiURL string
		want   bool
	}{
		{"exact panel origin", "https://panel.example.test", true},
		{"exact panel origin with api path", "https://panel.example.test/api", true},
		{"http downgrade rejected", "http://panel.example.test", false},
		{"foreign host rejected", "https://evil.test", false},
		{"foreign host with api path rejected", "https://evil.test/api", false},
		{"schemeless host rejected", "panel.example.test", false},
		{"empty rejected", "", false},
	}
	for _, c := range cases {
		if got := a.isPanelOrigin(c.apiURL); got != c.want {
			t.Errorf("%s: isPanelOrigin(%q) = %v, want %v", c.name, c.apiURL, got, c.want)
		}
	}
}

func TestSetSessionRejectsNonPanelOrigin(t *testing.T) {
	a := panelOriginTestApp(t)
	// Foreign host: rejected before any client is created.
	if err := a.SetSession("https://evil.test/api", "jwt"); err == nil {
		t.Error("SetSession accepted a foreign host - must reject")
	}
	// http downgrade of the panel host: rejected on the scheme mismatch.
	if err := a.SetSession("http://panel.example.test/api", "jwt"); err == nil {
		t.Error("SetSession accepted an http downgrade - must reject")
	}
	if a.getClient() != nil {
		t.Error("SetSession installed a client for a rejected origin")
	}
}

func TestLoginRejectsNonPanelOrigin(t *testing.T) {
	a := panelOriginTestApp(t)
	// Both rejection paths return before NewCoreClient, so no network is touched.
	if _, err := a.Login("https://evil.test", "u", "p"); err == nil {
		t.Error("Login accepted a foreign host - must reject")
	}
	if _, err := a.Login("http://panel.example.test", "u", "p"); err == nil {
		t.Error("Login accepted an http downgrade - must reject")
	}
	if a.getClient() != nil {
		t.Error("Login installed a client for a rejected origin")
	}
}

func TestApplyUpdateTokenGate(t *testing.T) {
	a := &App{shellToken: "good"}
	// Bad token must return before touching updateInFlight (no run started,
	// no network goroutine spawned).
	a.ApplyUpdate("bad")
	if a.updateInFlight.Load() {
		t.Error("ApplyUpdate started a run for a bad token - must reject first")
	}
}

func TestOpenUpdateDownloadTokenGate(t *testing.T) {
	// Smoke gate: a bad token must return immediately (before GetUpdateInfo's
	// network fetch and before BrowserOpenURL). We cannot intercept the native
	// browser-open here, so the deterministic gate coverage lives in
	// TestCheckShellToken; this only asserts the bad-token path is a no-op that
	// does not panic. ctx is nil, so even the token-valid path would no-op.
	a := &App{shellToken: "good"}
	a.OpenUpdateDownload("bad")
}

// TestFileURL covers the reveal-in-explorer URL construction. The old code
// concatenated "file://" with the path, which on Windows makes the drive letter
// the URL HOST (file://C:/x) and leaves spaces unescaped.
func TestFileURL(t *testing.T) {
	// Inputs go through FromSlash so each case is a NATIVE path on whichever OS
	// runs it: separators become backslashes on Windows (which is what ToSlash
	// has to undo) and stay as-is on the Linux CI runner. Hard-coding a
	// backslash instead would assert Windows behaviour on a platform where
	// ToSlash is a no-op, and fail there.
	cases := []struct{ in, want string }{
		{filepath.FromSlash("C:/Users/x/Downloads"), "file:///C:/Users/x/Downloads"},
		{filepath.FromSlash("C:/Users/x/My Documents"), "file:///C:/Users/x/My%20Documents"},
		{filepath.FromSlash("/home/x/Downloads"), "file:///home/x/Downloads"},
		{filepath.FromSlash("/home/x/my files"), "file:///home/x/my%20files"},
	}
	for _, c := range cases {
		if got := fileURL(c.in); got != c.want {
			t.Errorf("fileURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
