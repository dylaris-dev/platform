package packs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-core/handlers"
	"dylaris-core/services/corestorage"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/services"
)

// The pack mirror answers anyone, with no session. What it may serve is a
// build's pack.mrpack, and only behind an expiring signature Core made for that
// exact key: the unguessable path used to be the whole credential, with no
// expiry. Pinned through the real route pattern and a real local backend.
func TestModpackMirrorServesSignedPacksOnly(t *testing.T) {
	dir := t.TempDir()
	put := func(key, body string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const pack = "modpacks/abc123/pack.mrpack"
	put(pack, "BUILT-PACK")
	put("modpacks/abc123/server-ff.zip", "OLD-SERVER-PACK")
	put("packs/some-user/mods/private/private-1.0.zip", "SOMEONES-UPLOAD")
	put("solder/mods/some-user/a/a-1.0.zip", "LEFTOVER-SOLDER-MOD")

	paths, _ := json.Marshal([]string{dir})
	fs := &serverPackFakeStore{settings: map[string]string{
		"feature_modpacks_enabled": "true",
		"modpack_storage_provider": "local",
		"modpack_storage_paths":    string(paths),
	}}
	const secret = "cluster-secret-under-test"
	h := &PacksHandler{state: withCoreStorage(&handlers.AppState{Store: fs, FeatureFlags: services.NewFeatureFlags(fs), ClusterSecret: secret})}

	r := mux.NewRouter()
	r.HandleFunc("/mirror/{rest:.*}", h.ModpackMirror).Methods("GET")
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	sign := func(key string, now time.Time, secret string) string {
		return corestorage.SignedModpackMirrorURL("/mirror/", secret, key, now)
	}

	if rec := get(sign(pack, time.Now(), secret)); rec.Code != http.StatusOK || rec.Body.String() != "BUILT-PACK" {
		t.Fatalf("a signed pack was not served: %d %q - a node could not install it", rec.Code, rec.Body.String())
	}

	signed := sign(pack, time.Now(), secret)
	cases := []struct{ name, path string }{
		{"unsigned, the old URL shape", "/mirror/" + pack},
		{"expired", sign(pack, time.Now().Add(-corestorage.ModpackMirrorTTL-time.Minute), secret)},
		{"signed by another secret", sign(pack, time.Now(), "some-other-secret")},
		{"signature of another key", strings.Replace(signed, "abc123", "abc124", 1)},
		{"tampered expiry", strings.Replace(signed, "exp=", "exp=9", 1)},
		{"signed server pack", sign("modpacks/abc123/server-ff.zip", time.Now(), secret)},
		{"signed upload", sign("packs/some-user/mods/private/private-1.0.zip", time.Now(), secret)},
		{"signed solder leftover", sign("solder/mods/some-user/a/a-1.0.zip", time.Now(), secret)},
		{"traversal", sign("modpacks/../packs/some-user/mods/private/private-1.0.zip", time.Now(), secret)},
		{"bare prefix", "/mirror/modpacks"},
	}
	for _, c := range cases {
		rec := get(c.path)
		// 404 everywhere; the router answers a ".." path with a redirect to
		// its cleaned form before the handler sees it, which is fine too.
		if rec.Code != http.StatusNotFound && !(c.name == "traversal" && rec.Code == http.StatusMovedPermanently) {
			t.Errorf("%s: answered %d, want 404", c.name, rec.Code)
		}
		for _, leak := range []string{"BUILT-PACK", "OLD-SERVER-PACK", "SOMEONES-UPLOAD", "LEFTOVER-SOLDER-MOD"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("%s leaked %s through the anonymous mirror", c.name, leak)
			}
		}
	}
}

// The install URL stays on Core's public host with the key as its path, which
// is all an older node checks before downloading.
func TestSignedMirrorURLKeepsHostAndPath(t *testing.T) {
	u := corestorage.SignedModpackMirrorURL("https://core.example.com/mirror/", "s", "modpacks/abc/pack.mrpack", time.Now())
	if !strings.HasPrefix(u, "https://core.example.com/mirror/modpacks/abc/pack.mrpack?exp=") || !strings.Contains(u, "&sig=") {
		t.Fatalf("signed URL = %q", u)
	}
}

// A build published on Modrinth installs from Modrinth's own .mrpack; when
// Modrinth does not answer for it, or for an unpublished build, the node gets
// Core's signed mirror URL instead.
func TestInstallMrpackURLPrefersModrinth(t *testing.T) {
	fs := &serverPackFakeStore{settings: map[string]string{"core_public_url": "https://core.example.com/"}}
	h := &PacksHandler{state: withCoreStorage(&handlers.AppState{Store: fs, ClusterSecret: "s"})}
	pack := &models.Pack{ID: 1, ModrinthProjectID: "proj"}
	const cdn = "https://cdn.modrinth.com/data/proj/versions/v1/p.mrpack"

	old := fetchPublishedVersion
	t.Cleanup(func() { fetchPublishedVersion = old })
	for _, c := range []struct {
		name      string
		published bool
		version   *services.ModrinthVersion
		err       error
		modrinth  bool
	}{
		{"published", true, &services.ModrinthVersion{ProjectID: "proj", Files: []services.ModrinthFile{{Filename: "p.mrpack", URL: cdn, Primary: true}}}, nil, true},
		{"modrinth unreachable", true, nil, errors.New("404"), false},
		{"another project's version", true, &services.ModrinthVersion{ProjectID: "other", Files: []services.ModrinthFile{{Filename: "p.mrpack", URL: cdn, Primary: true}}}, nil, false},
		{"not on the CDN", true, &services.ModrinthVersion{ProjectID: "proj", Files: []services.ModrinthFile{{Filename: "p.mrpack", URL: "https://evil.example/p.mrpack", Primary: true}}}, nil, false},
		{"not published", false, nil, nil, false},
	} {
		fetchPublishedVersion = func(context.Context, string) (*services.ModrinthVersion, error) { return c.version, c.err }
		build := &models.PackBuild{ID: 1, ModrinthPublished: c.published, ModrinthVersionID: "v1", MrpackStorageKey: "modpacks/abc/pack.mrpack"}
		u, err := h.InstallMrpackURL(context.Background(), pack, build)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.modrinth && u != cdn {
			t.Errorf("%s: url %q, want Modrinth's file", c.name, u)
		}
		if !c.modrinth && !strings.HasPrefix(u, "https://core.example.com/mirror/modpacks/abc/pack.mrpack?exp=") {
			t.Errorf("%s: url %q, want Core's signed mirror", c.name, u)
		}
	}
}
