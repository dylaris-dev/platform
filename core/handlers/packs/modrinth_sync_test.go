package packs

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
)

func TestNewModrinthVersionsOnlyAfterTheNewestImported(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	at := func(d int) *time.Time { x := day(d); return &x }
	versions := []services.ModrinthProjectVersion{
		{ID: "v5", DatePublished: day(25)}, {ID: "v4", DatePublished: day(20)},
		{ID: "v3", DatePublished: day(15)}, {ID: "v2", DatePublished: day(10)}, {ID: "v1", DatePublished: day(5)},
	}
	// The owner imported only v3: v1 and v2 are older and stay missing.
	builds := []models.PackBuild{{ID: 1, ModrinthVersionID: "v3", PublishedAt: at(15)}, {ID: 2}}
	got := newModrinthVersions(versions, builds)
	if len(got) != 2 || got[0].ID != "v4" || got[1].ID != "v5" {
		t.Fatalf("got %v, want v4 then v5", got)
	}
	if got := newModrinthVersions(versions, []models.PackBuild{{ID: 2}}); len(got) != 0 {
		t.Fatalf("a pack without any imported version synced %v automatically", got)
	}
}

type syncStore struct{ *importStore }

func (s *syncStore) ListModrinthBoundPacks() ([]models.Pack, error) {
	return []models.Pack{*s.pack}, nil
}

// A version that can never import (here: it has no .mrpack) stopped the sync
// for that pack at that version on every pass, so no newer one ever arrived.
func TestSyncSkipsAVersionThatCanNeverImport(t *testing.T) {
	h, base, _ := newPackCleanup(t)
	base.pack.ModrinthProjectID = "proj"
	first := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	base.builds = []models.PackBuild{{ID: 10, PackID: 1, VersionString: "1", ModrinthVersionID: "v1", PublishedAt: &first}}
	fs := &importStore{packCleanupStore: base, targets: map[string]string{}}
	h.d.Store = &syncStore{fs}
	h.d.CoreStorage.Store = h.d.Store
	h.SetPATLoader(&publishBindingPAT{})

	pack := zipOf(t, map[string]string{"modrinth.index.json": `{"name":"P","dependencies":{"minecraft":"1.20.1"},"files":[]}`})
	sum := sha1.Sum(pack)
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = modrinthAddTransport(func(r *http.Request) (*http.Response, error) {
		ok := func(body string) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		switch {
		case r.URL.Host == "api.modrinth.com" && r.URL.Path == "/v2/project/proj/version":
			return ok(`[{"id":"v3","project_id":"proj","version_number":"3","date_published":"2026-10-03T00:00:00Z",
			  "files":[{"filename":"p.mrpack","url":"https://cdn.modrinth.com/data/proj/versions/v3/p.mrpack","primary":true,"hashes":{"sha1":"` + hex.EncodeToString(sum[:]) + `"}}]},
			 {"id":"v2","project_id":"proj","version_number":"2","date_published":"2026-10-02T00:00:00Z","files":[]},
			 {"id":"v1","project_id":"proj","version_number":"1","date_published":"2026-10-01T00:00:00Z","files":[]}]`)
		case r.URL.Host == "cdn.modrinth.com" && strings.HasSuffix(r.URL.Path, "/v3/p.mrpack"):
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(pack))}, nil
		case r.URL.Host == "api.modrinth.com" && r.URL.Path == "/v2/versions":
			return ok(`[]`)
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})

	h.syncModrinthPacks(context.Background())
	if fs.final == nil || fs.final.ModrinthVersionID != "v3" {
		t.Fatalf("final = %+v, want v3 imported past the unimportable v2", fs.final)
	}
}
