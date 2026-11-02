package packs

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-core/models"
)

// replaceStore records the rewritten modversion and reports no other row on
// the old object, so it may be deleted.
type replaceStore struct {
	*serverPackFakeStore
	updated *models.Modversion
}

func (s *replaceStore) UpdateModversion(mv *models.Modversion) error {
	c := *mv
	s.updated = &c
	return nil
}
func (s *replaceStore) CountModversionsByStorageKey(string) (int, error) {
	return 0, nil
}

// Replace-with-Modrinth turns a stored upload into a reference to Modrinth's
// file: nothing is downloaded from the CDN, nothing is stored, and the old
// stored copy goes. Dylaris must not keep copies of files Modrinth distributes.
func TestReplaceWithModrinthStoresAReferenceOnly(t *testing.T) {
	dir := t.TempDir()
	h := localModpackHandler(t, dir)
	st := &replaceStore{serverPackFakeStore: h.state.Store.(*serverPackFakeStore)}
	h.state.Store = st
	h.state.CoreStorage.Store = h.state.Store
	storeZip(t, dir, "packs/u/mods/sodium/old.zip", "mods/sodium.jar", []byte("uploaded copy"))

	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = modrinthAddTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.modrinth.com" && r.URL.Path == "/v2/version/v2" {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(
				`{"id":"v2","project_id":"sodium","version_number":"0.6","loaders":["fabric"],"game_versions":["1.20.1"],
				 "files":[{"filename":"sodium.jar","url":"https://cdn.modrinth.com/data/sodium/versions/v2/sodium.jar","primary":true,"size":42,"hashes":{"sha1":"s1","sha512":"s5"}}]}`))}, nil
		}
		t.Errorf("unexpected request %s %s: a reference needs no download", r.Method, r.URL)
		return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})

	build := &models.PackBuild{ID: 1, PackID: 1, Minecraft: "1.20.1", Loader: "fabric"}
	mv := &models.Modversion{ID: 7, StorageKey: "packs/u/mods/sodium/old.zip", MD5: "m", Source: models.SourceUpload, TargetPath: "mods/sodium.jar"}
	if err := h.swapModversionToModrinth(context.Background(), "u", build, mv, "v2"); err != nil {
		t.Fatal(err)
	}
	got := st.updated
	if got == nil || got.StorageKey != "" || got.MD5 != "" || got.Source != models.SourceModrinth ||
		got.ModrinthDownloadURL != "https://cdn.modrinth.com/data/sodium/versions/v2/sodium.jar" ||
		got.SHA1 != "s1" || got.SHA512 != "s5" || got.Filesize != 42 || got.ModrinthProjectID != "sodium" || got.TargetPath != "mods/sodium.jar" {
		t.Fatalf("rewritten modversion = %+v", got)
	}
	if !isMrpackFilesEntry(models.BuildContentEntry{Modversion: *got, Linked: true}) {
		t.Error("the reference would not be a files[] entry and would leave the pack")
	}
	if _, err := os.Stat(filepath.Join(dir, "packs/u/mods/sodium/old.zip")); !os.IsNotExist(err) {
		t.Errorf("the old stored copy survived: %v", err)
	}
	var stored []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			stored = append(stored, p)
		}
		return nil
	})
	if len(stored) != 0 {
		t.Errorf("replace left objects in storage: %v", stored)
	}
}
