package handlers

import (
	"os"
	"strings"
	"testing"
)

// The storage key must never carry raw remote text. version_number is whatever
// the project author typed on Modrinth and it went into the key verbatim. A key
// that escapes "packs/<owner>/mods/<slug>/" writes into another
// owner's prefix.
//
// The key is built inline in swapModversionToModrinth, so this pins the
// slugify + md5 shape it now uses rather than reaching through a network call.
func TestAModrinthVersionNumberCannotEscapeTheStorageKey(t *testing.T) {
	const owner = "11111111-1111-4111-8111-111111111111"
	const md5hex = "0123456789abcdef0123456789abcdef"

	// Mirrors the key construction under test.
	key := func(slug, versionNum string) string {
		keyVersion := slugify(versionNum)
		if keyVersion == "" {
			keyVersion = "v"
		}
		keyVersion = keyVersion + "-" + md5hex[:8]
		return "packs/" + owner + "/mods/" + slug + "/" + slug + "-" + keyVersion + ".zip"
	}

	prefix := "packs/" + owner + "/mods/"
	for _, versionNum := range []string{
		"../../../../victim/mods/sodium/sodium-1.0",
		"1.0/../../..",
		`..\..\evil`,
		"/absolute",
		"..",
		"",
	} {
		got := key("sodium", versionNum)
		if !strings.HasPrefix(got, prefix) {
			t.Errorf("version %q produced a key outside the owner prefix: %s", versionNum, got)
		}
		if strings.Contains(got, "..") {
			t.Errorf("version %q left a traversal in the key: %s", versionNum, got)
		}
		if strings.Count(strings.TrimPrefix(got, prefix), "/") != 1 {
			t.Errorf("version %q changed the key depth: %s", versionNum, got)
		}
	}

	// An ordinary version still produces a readable, unique key.
	if got, want := key("sodium", "0.5.3"), prefix+"sodium/sodium-0-5-3-01234567.zip"; got != want {
		t.Errorf("ordinary key = %q, want %q", got, want)
	}
}

// Every place that fetches a Modrinth file URL must go through the CDN
// allowlist. This one had its own http.Client and no host check, so whatever
// URL the Modrinth API named in files[].url was fetched by Core from inside the
// network. The allowlist exists precisely so a third party's response cannot
// steer a download.
func TestReplaceGoesThroughTheGuardedModrinthDownloader(t *testing.T) {
	raw, err := os.ReadFile("packs_replace.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	src := string(raw)

	if strings.Contains(src, "replaceDownloadClient") || strings.Contains(src, "func downloadURL(") {
		t.Error("packs_replace.go builds its own HTTP client again; that path has no cdn.modrinth.com check")
	}
	if !strings.Contains(src, "services.DownloadModrinthJar(ctx, file.URL") {
		t.Error("swapModversionToModrinth no longer downloads through services.DownloadModrinthJar")
	}
	// The downloader verifies only the hashes it is handed, so the caller has to
	// insist that at least one exists or an unverifiable file is stored as
	// "byte-identical to Modrinth".
	if !strings.Contains(src, `file.Hashes["sha1"] == "" && file.Hashes["sha512"] == ""`) {
		t.Error("the requirement that Modrinth published at least one hash is gone")
	}
	// The key test above mirrors the construction rather than calling it, so it
	// cannot notice the real one drifting back. Pin the real one here: the raw
	// version_number must not be concatenated into a storage key.
	if !strings.Contains(src, "keyVersion := slugify(v.VersionNum)") {
		t.Error("the Modrinth version_number is no longer slugified for the storage key")
	}
	if strings.Contains(src, `+ "-" + v.VersionNum + ".zip"`) {
		t.Error("the raw Modrinth version_number is back in the storage key")
	}
}
