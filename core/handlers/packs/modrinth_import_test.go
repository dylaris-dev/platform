package packs

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
)

// importStore records what an import writes.
type importStore struct {
	*packCleanupStore
	created  *models.PackBuild
	final    *models.PackBuild
	deleted  []int
	targets  map[string]string // target path -> side
	nextMVID int
}

func (s *importStore) CreatePackBuild(b *models.PackBuild) (int, error) {
	c := *b
	s.created = &c
	return 20, nil
}
func (s *importStore) UpdatePackBuild(b *models.PackBuild) error { c := *b; s.final = &c; return nil }
func (s *importStore) DeletePackBuild(id, _ int) error           { s.deleted = append(s.deleted, id); return nil }
func (s *importStore) CreateModversion(v *models.Modversion) (int, error) {
	s.nextMVID++
	path := v.TargetPath
	if v.ModrinthVersionID != "" {
		path = "modrinth:" + v.ModrinthVersionID
	}
	s.targets[path] = ""
	return s.nextMVID, nil
}
func (s *importStore) AttachModversionToBuild(_, mvID int, side string) (int, error) {
	// The last created modversion is the one attached.
	for k, v := range s.targets {
		if v == "" {
			s.targets[k] = side
		}
	}
	return mvID, nil
}
func (s *importStore) AddPackAudit(string, int, string, string, string, map[string]any) error {
	return nil
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImportModrinthVersionBuildsAFrozenImportedBuild(t *testing.T) {
	h, base, _ := newPackCleanup(t)
	base.pack.ModrinthProjectID = "proj"
	fs := &importStore{packCleanupStore: base, targets: map[string]string{}}
	h.state.Store = fs
	h.SetPATLoader(&publishBindingPAT{})

	pack := zipOf(t, map[string]string{
		"modrinth.index.json": `{"name":"P","dependencies":{"minecraft":"1.20.1","forge":"47.4.28"},"files":[
		 {"path":"mods/jei.jar","hashes":{"sha1":"aa"},"env":{"client":"required","server":"required"},"downloads":["https://cdn.modrinth.com/data/jei/versions/abc123/jei.jar"]},
		 {"path":"mods/skin.jar","hashes":{"sha1":"aa"},"env":{"client":"required","server":"unsupported"},"downloads":["https://cdn.modrinth.com/data/skin/versions/def456/skin.jar"]}]}`,
		"overrides/config/jei.toml":    "a=1",
		"client-overrides/options.txt": "fov:90",
	})
	sum := sha1.Sum(pack)
	packSHA1 := hex.EncodeToString(sum[:])

	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = modrinthAddTransport(func(r *http.Request) (*http.Response, error) {
		ok := func(body string) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		switch {
		case r.URL.Host == "api.modrinth.com" && r.URL.Path == "/v2/project/proj/version":
			return ok(`[{"id":"v1","project_id":"proj","version_number":"1.0.0","version_type":"release","changelog":"First","date_published":"2026-01-02T03:04:05Z","game_versions":["1.20.1"],"loaders":["forge"],
			 "files":[{"filename":"p.mrpack","url":"https://cdn.modrinth.com/data/proj/versions/v1/p.mrpack","primary":true,"hashes":{"sha1":"` + packSHA1 + `"}}]}]`)
		case r.URL.Host == "cdn.modrinth.com" && strings.HasSuffix(r.URL.Path, "/p.mrpack"):
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(pack))}, nil
		case r.URL.Host == "api.modrinth.com" && r.URL.Path == "/v2/versions":
			// skin.jar's version answers with a different file hash: it must be
			// treated as not that version's file, never linked blindly.
			return ok(`[{"id":"abc123","project_id":"jei","version_number":"15","loaders":["forge"],"game_versions":["1.20.1"],"files":[{"filename":"jei.jar","url":"https://cdn.modrinth.com/data/jei/versions/abc123/jei.jar","primary":true,"size":3,"hashes":{"sha1":"aa","sha512":"bb"}}]},
			 {"id":"def456","project_id":"skin","version_number":"1","loaders":["forge"],"game_versions":["1.20.1"],"files":[{"filename":"skin.jar","url":"https://cdn.modrinth.com/data/skin/versions/def456/skin.jar","primary":true,"size":3,"hashes":{"sha1":"ff","sha512":"ee"}}]}]`)
		case r.URL.Host == "cdn.modrinth.com" && strings.HasSuffix(r.URL.Path, "/skin.jar"):
			return ok("not the checksum")
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})

	w := httptest.NewRecorder()
	h.ImportModrinthVersion(w, packReq(http.MethodPost, map[string]string{"id": "1", "versionId": "v1"}, ""))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if fs.created == nil || fs.created.ImportedFrom != "modrinth" || fs.created.VersionString != "1.0.0" || fs.created.Loader != "forge" || fs.created.LoaderVersion != "47.4.28" || fs.created.Minecraft != "1.20.1" {
		t.Fatalf("created build = %+v", fs.created)
	}
	f := fs.final
	if f == nil || !f.Frozen || !f.ModrinthPublished || f.ModrinthVersionID != "v1" || f.Channel != models.ChannelRelease || f.MrpackStorageKey == "" || f.PublishedAt == nil {
		t.Fatalf("final build = %+v", f)
	}
	if fs.targets["modrinth:abc123"] != "both" || fs.targets["config/jei.toml"] != "both" || fs.targets["options.txt"] != "client" {
		t.Fatalf("content = %v", fs.targets)
	}
	if _, linked := fs.targets["modrinth:def456"]; linked {
		t.Fatal("a file whose checksum differs from the version's file was linked anyway")
	}
	if !strings.Contains(w.Body.String(), "skin.jar did not match its checksum") || len(fs.deleted) != 0 {
		t.Fatalf("body=%s deleted=%v", w.Body.String(), fs.deleted)
	}
}

func TestImportModrinthVersionRefusesAVersionOfAnotherProject(t *testing.T) {
	h, base, _ := newPackCleanup(t)
	base.pack.ModrinthProjectID = "proj"
	h.state.Store = &importStore{packCleanupStore: base, targets: map[string]string{}}
	h.SetPATLoader(&publishBindingPAT{})
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = modrinthAddTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`[{"id":"v1","files":[]}]`))}, nil
	})
	w := httptest.NewRecorder()
	h.ImportModrinthVersion(w, packReq(http.MethodPost, map[string]string{"id": "1", "versionId": "someone-elses"}, ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for a version outside the pack's project", w.Code)
	}
}
