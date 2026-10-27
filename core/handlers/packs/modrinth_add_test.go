package packs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
)

type modrinthAddTransport func(*http.Request) (*http.Response, error)

func (f modrinthAddTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestModrinthAddKeepsExistingDependencies(t *testing.T) {
	h, base, _ := newPackCleanup(t)
	base.builds[0].Minecraft = "1.20.1"
	base.builds[0].Loader = "forge"
	fs := &authoringCaptureStore{packCleanupStore: base, existing: []models.BuildContentEntry{{Modversion: models.Modversion{ModrinthProjectID: "dependency"}, Side: models.SideBoth}}}
	h.state.Store = fs
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	calls := 0
	http.DefaultTransport = modrinthAddTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.modrinth.com" || r.URL.Path != "/v2/version/root-version" {
			t.Fatalf("existing dependency fetched again: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"root-version","project_id":"root-project","name":"Root","version_number":"1.0","loaders":["forge"],"game_versions":["1.20.1"],"files":[{"filename":"root.jar","url":"https://cdn.modrinth.com/data/root.jar","hashes":{"sha1":"a","sha512":"b"},"size":10}],"dependencies":[{"project_id":"dependency","dependency_type":"required"}]}`))}, nil
	})
	w := httptest.NewRecorder()
	h.AddModrinth(w, packReq("POST", map[string]string{"id": "1", "buildId": "10"}, `{"versionId":"root-version","resolveDeps":true}`))
	if w.Code != 200 || calls != 1 || len(fs.mods) != 1 {
		t.Fatalf("status=%d calls=%d mods=%d body=%s", w.Code, calls, len(fs.mods), w.Body.String())
	}
	// A repeated add is an explicit conflict, not another same-path ZIP entry.
	fs.existing = append(fs.existing, models.BuildContentEntry{Modversion: models.Modversion{ModrinthProjectID: "root-project"}, Side: models.SideBoth})
	w = httptest.NewRecorder()
	h.AddModrinth(w, packReq("POST", map[string]string{"id": "1", "buildId": "10"}, `{"versionId":"root-version"}`))
	if w.Code != 409 || len(fs.mods) != 1 {
		t.Fatalf("repeated add status=%d mods=%d", w.Code, len(fs.mods))
	}
}
