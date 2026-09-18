package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/services"
)

// The pack mirror answers anyone, with no credential, and the prefix check is
// the WHOLE boundary between it and every object in the modpack bucket: users'
// uploaded mods, and whatever Solder left behind under solder/ and loaders/.
// Widening that check would leave every other test green, so it is pinned here
// through the real route pattern and a real local backend holding real files.
func TestModpackMirrorServesBuiltPacksOnly(t *testing.T) {
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
	put("modpacks/abc123/pack.mrpack", "BUILT-PACK")
	put("packs/some-user/mods/private/private-1.0.zip", "SOMEONES-UPLOAD")
	put("solder/mods/some-user/a/a-1.0.zip", "LEFTOVER-SOLDER-MOD")
	put("solder/manifests/some-user/p/1.0/build.json", "LEFTOVER-MANIFEST")
	put("loaders/fabric/1.21/0.16/loader.zip", "LEFTOVER-LOADER")

	paths, _ := json.Marshal([]string{dir})
	fs := &serverPackFakeStore{settings: map[string]string{
		"feature_modpacks_enabled": "true",
		"modpack_storage_provider": "local",
		"modpack_storage_paths":    string(paths),
	}}
	h := &PacksHandler{state: &AppState{Store: fs, FeatureFlags: services.NewFeatureFlags(fs)}}

	r := mux.NewRouter()
	r.HandleFunc("/mirror/{rest:.*}", h.ModpackMirror).Methods("GET")

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	if rec := get("/mirror/modpacks/abc123/pack.mrpack"); rec.Code != http.StatusOK || rec.Body.String() != "BUILT-PACK" {
		t.Fatalf("a built pack was not served: %d %q - a node could not install it", rec.Code, rec.Body.String())
	}

	secrets := []string{"SOMEONES-UPLOAD", "LEFTOVER-SOLDER-MOD", "LEFTOVER-MANIFEST", "LEFTOVER-LOADER"}
	for _, path := range []string{
		"/mirror/packs/some-user/mods/private/private-1.0.zip",
		"/mirror/solder/mods/some-user/a/a-1.0.zip",
		"/mirror/solder/manifests/some-user/p/1.0/build.json",
		"/mirror/loaders/fabric/1.21/0.16/loader.zip",
		"/mirror/modpacks",
		"/mirror/",
		"/mirror/modpacks/../packs/some-user/mods/private/private-1.0.zip",
		"/mirror/modpacks/%2e%2e/packs/some-user/mods/private/private-1.0.zip",
	} {
		rec := get(path)
		if rec.Code == http.StatusOK {
			t.Errorf("%s answered 200; the anonymous mirror serves modpacks/ keys only", path)
		}
		for _, s := range secrets {
			if strings.Contains(rec.Body.String(), s) {
				t.Errorf("%s leaked %s through the anonymous mirror", path, s)
			}
		}
	}
}
