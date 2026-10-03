package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func isolateUserDirs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("AppData", filepath.Join(dir, "roaming"))
	t.Setenv("LocalAppData", filepath.Join(dir, "local"))
	t.Setenv("HOME", dir)
	return dir
}

// The session left the roaming profile: %AppData% is copied to every machine
// the user signs in to, and a live session token does not belong in it.
func TestTheSessionIsNotInTheRoamingProfile(t *testing.T) {
	isolateUserDirs(t)
	if filepath.Dir(sessionPath()) == filepath.Dir(settingsPath()) {
		t.Fatalf("the session sits beside config.json in %s", filepath.Dir(settingsPath()))
	}
}

// On Windows the file is sealed to the user account; the token must not be
// readable from the bytes on disk.
func TestTheSessionFileIsSealedWhereItCanBe(t *testing.T) {
	isolateUserDirs(t)
	want := storedSessions{Cookies: map[string][]storedCookie{"https://panel.example.com": {{Name: "s", Value: "secret-token-value"}}}}
	if err := writeStoredSessions(want); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(sessionPath())
	if err != nil {
		t.Fatal(err)
	}
	if sessionSealed && bytes.Contains(raw, []byte("secret-token-value")) {
		t.Fatal("the session token is readable in the file")
	}
	got := readStoredSessions()
	if c := got.Cookies["https://panel.example.com"]; len(c) != 1 || c[0].Value != "secret-token-value" {
		t.Fatalf("round trip lost the session: %+v", got)
	}
}

// A session written by an earlier version is taken over once and its plaintext
// copy removed, so updating Beam neither signs the user out nor leaves the
// token behind in the roaming profile.
func TestALegacySessionIsMovedNotLeftBehind(t *testing.T) {
	isolateUserDirs(t)
	legacy := legacySessionPath()
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"cookies":{"https://panel.example.com":[{"name":"s","value":"old-token"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readStoredSessions()
	if c := got.Cookies["https://panel.example.com"]; len(c) != 1 || c[0].Value != "old-token" {
		t.Fatalf("the earlier session was not taken over: %+v", got)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("the plaintext session from the earlier version is still there")
	}
	if got := readStoredSessions(); len(got.Cookies["https://panel.example.com"]) != 1 {
		t.Fatal("the moved session did not survive the next read")
	}
}
