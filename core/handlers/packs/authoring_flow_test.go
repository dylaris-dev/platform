package packs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
)

type authoringCaptureStore struct {
	*packCleanupStore
	savedVersion *models.Modversion
	savedBuild   *models.PackBuild
	existing     []models.BuildContentEntry
	mods         []models.Mod
	sides        []string
}

func (s *authoringCaptureStore) UpsertMod(m *models.Mod) (int, error) {
	s.mods = append(s.mods, *m)
	return len(s.mods), nil
}

func (s *authoringCaptureStore) ListBuildContent(int) ([]models.BuildContentEntry, error) {
	return s.existing, nil
}

func (s *authoringCaptureStore) CreateModversion(v *models.Modversion) (int, error) {
	cp := *v
	s.savedVersion = &cp
	return 7, nil
}
func (s *authoringCaptureStore) AttachModversionToBuild(_, _ int, side string) (int, error) {
	s.sides = append(s.sides, side)
	return 1, nil
}
func (s *authoringCaptureStore) UpdatePackBuild(b *models.PackBuild) error {
	cp := *b
	s.savedBuild = &cp
	return nil
}

func (s *packCleanupStore) ListBuildContent(int) ([]models.BuildContentEntry, error) { return nil, nil }

func TestBuildPatchPreservesMetadata(t *testing.T) {
	h, fs, _ := newPackCleanup(t)
	fs.builds[0].Minecraft = "1.20.1"
	fs.builds[0].Loader = "forge"
	fs.builds[0].LoaderVersion = "47.4.28"
	fs.builds[0].Changelog = "Original notes"
	capture := &authoringCaptureStore{packCleanupStore: fs}
	h.state.Store = capture
	w := httptest.NewRecorder()
	h.UpdateBuild(w, packReq("PATCH", map[string]string{"id": "1", "buildId": "10"}, `{"changelog":"Updated notes"}`))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	b := capture.savedBuild
	if b == nil || b.Minecraft != "1.20.1" || b.Loader != "forge" || b.LoaderVersion != "47.4.28" || b.Changelog != "Updated notes" {
		t.Fatalf("metadata lost: %+v", b)
	}
	h.UpdateBuild(httptest.NewRecorder(), packReq("PATCH", map[string]string{"id": "1", "buildId": "10"}, `{"changelog":""}`))
	if capture.savedBuild.Changelog != "" {
		t.Fatal("explicit empty changelog did not clear")
	}
}

func TestMigrationRejectsUnsafeMetadataBeforeResolving(t *testing.T) {
	for _, body := range []string{`{"versionString":"next","minecraft":"../other","loaderVersion":"47.4.28"}`, `{"versionString":"next","minecraft":"1.20.2","loaderVersion":"../other"}`} {
		h, fs, _ := newPackCleanup(t)
		fs.builds[0].Loader = "forge"
		fs.builds[0].Minecraft = "1.20.1"
		w := httptest.NewRecorder()
		h.MigrateBuild(w, packReq("POST", map[string]string{"id": "1", "buildId": "10"}, body))
		if w.Code != 400 {
			t.Fatalf("unsafe migration status=%d: %s", w.Code, w.Body.String())
		}
	}
}

func TestUploadPreservesConfigAndScriptPaths(t *testing.T) {
	tests := []struct {
		name, target string
		archive      bool
		data         []byte
		wantPath     string
	}{
		{"script.js", "kubejs/server_scripts/recipes.js", false, []byte("ServerEvents.recipes(event => {});"), "kubejs/server_scripts/recipes.js"},
		{"settings.toml", "defaultconfigs/example/settings.toml", false, []byte("enabled=true"), "defaultconfigs/example/settings.toml"},
		{"configs.zip", "", true, zipBytes(t, map[string]string{"config/example.toml": "enabled=true", "kubejs/startup_scripts/items.js": "// test"}), "kubejs/startup_scripts/items.js"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, fs, dir := newPackCleanup(t)
			capture := &authoringCaptureStore{packCleanupStore: fs}
			h.state.Store = capture
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			part, err := mw.CreateFormFile("file", tt.name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = part.Write(tt.data)
			_ = mw.WriteField("contentType", models.ContentTypeConfig)
			_ = mw.WriteField("side", models.SideBoth)
			if tt.target != "" {
				_ = mw.WriteField("targetPath", tt.target)
			}
			if tt.archive {
				_ = mw.WriteField("archive", "true")
			}
			_ = mw.Close()
			r := packReq("POST", map[string]string{"id": "1", "buildId": "10"}, "")
			r.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
			r.Header.Set("Content-Type", mw.FormDataContentType())
			w := httptest.NewRecorder()
			h.UploadContent(w, r)
			if w.Code != 200 {
				t.Fatalf("upload status=%d: %s", w.Code, w.Body.String())
			}
			if capture.savedVersion == nil {
				t.Fatal("version not persisted")
			}
			prov := modpackProviderAt(t, dir)
			raw, err := prov.Get(r.Context(), capture.savedVersion.StorageKey)
			if err != nil {
				t.Fatal(err)
			}
			zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range zr.File {
				if f.Name == tt.wantPath {
					found = true
				}
			}
			if !found {
				t.Fatalf("stored archive lost %q", tt.wantPath)
			}
			if tt.target != "" && capture.savedVersion.TargetPath != tt.target {
				t.Fatalf("target path=%q", capture.savedVersion.TargetPath)
			}
		})
	}
}

func TestGetPackReturnsEffectivePermissions(t *testing.T) {
	fs := &packAccessStore{members: map[string]*models.ModrinthMembership{
		"manager": member(services.ModrinthPermAll), "uploader": member(services.ModrinthPermUploadVersion), "viewer": member(0),
		"lapsed": lapsed(time.Hour),
	}}
	h := NewPacksHandler(&handlers.AppState{Store: fs})
	for _, tt := range []struct {
		user                                    string
		edit, delete, page, deletePack, invalid bool
	}{{"manager", true, true, true, true, false}, {"uploader", true, false, false, false, false}, {"viewer", false, false, false, false, false}, {"lapsed", false, false, false, false, true}} {
		r := packPatchRequest("/api/packs/1", "")
		r = r.WithContext(context.WithValue(r.Context(), "userID", tt.user))
		w := httptest.NewRecorder()
		h.Get(w, r)
		var reply struct {
			Permissions struct {
				CanEdit       bool `json:"canEdit"`
				CanDelete     bool `json:"canDelete"`
				CanEditPage   bool `json:"canEditPage"`
				CanDeletePack bool `json:"canDeletePack"`
				TokenInvalid  bool `json:"tokenInvalid"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		p := reply.Permissions
		if p.CanEdit != tt.edit || p.CanDelete != tt.delete || p.CanEditPage != tt.page || p.CanDeletePack != tt.deletePack || p.TokenInvalid != tt.invalid {
			t.Fatalf("%s permissions=%+v", tt.user, p)
		}
	}
}

type deleteContentRightsStore struct {
	*packCleanupStore
	membership *models.ModrinthMembership
	detached   bool
}

func (s *deleteContentRightsStore) GetModrinthMembership(string, string) (*models.ModrinthMembership, error) {
	return s.membership, nil
}
func (s *deleteContentRightsStore) DetachFromBuild(int, int) error { s.detached = true; return nil }

// Removing content needs Modrinth's "delete version" right, as deleting did
// with the old invite's delete right; uploading alone is not enough.
func TestDeletingContentRequiresTheDeleteVersionRight(t *testing.T) {
	for _, tt := range []struct {
		name   string
		perms  int64
		status int
	}{{"upload is not delete", services.ModrinthPermUploadVersion, 403}, {"delete version", services.ModrinthPermDeleteVersion, 200}, {"read only", 0, 403}} {
		t.Run(tt.name, func(t *testing.T) {
			h, base, _ := newPackCleanup(t)
			base.pack.ModrinthProjectID = "proj"
			fs := &deleteContentRightsStore{packCleanupStore: base, membership: member(tt.perms)}
			h.state.Store = fs
			r := packReq("DELETE", map[string]string{"id": "1", "buildId": "10", "modversionId": "7"}, "")
			r = r.WithContext(context.WithValue(r.Context(), "userID", "teammate"))
			w := httptest.NewRecorder()
			h.RemoveContent(w, r)
			if w.Code != tt.status || fs.detached != (tt.status == 200) {
				t.Fatalf("status=%d detached=%v want status=%d", w.Code, fs.detached, tt.status)
			}
		})
	}
}

func uploadAuthoringConfig(t *testing.T, h *PacksHandler, packID, buildID int, kind string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	file, err := mw.CreateFormFile("file", "same.js")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write(data)
	_ = mw.WriteField("contentType", kind)
	_ = mw.WriteField("targetPath", "kubejs/server_scripts/same.js")
	_ = mw.Close()
	r := packReq("POST", map[string]string{"id": strconv.Itoa(packID), "buildId": strconv.Itoa(buildID)}, "")
	r.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.UploadContent(w, r)
	return w
}

func TestCustomUploadCatalogIsScopedToTheBuild(t *testing.T) {
	h, base, dir := newPackCleanup(t)
	fs := &authoringCaptureStore{packCleanupStore: base}
	h.state.Store = fs
	firstBytes := []byte("// original build")
	if w := uploadAuthoringConfig(t, h, 1, 10, models.ContentTypeConfig, firstBytes); w.Code != 200 {
		t.Fatalf("first upload: %d %s", w.Code, w.Body.String())
	}
	first := *fs.savedVersion
	original := fs.mods[0]
	base.builds = append(base.builds, models.PackBuild{ID: 12, PackID: 1})
	if w := uploadAuthoringConfig(t, h, 1, 12, models.ContentTypeConfig, []byte("// another build")); w.Code != 200 {
		t.Fatalf("second upload: %d %s", w.Code, w.Body.String())
	}
	if fs.mods[1].Slug == original.Slug || fs.savedVersion.StorageKey == first.StorageKey {
		t.Fatal("a second build reused the first build's mutable catalog or artifact")
	}
	if w := uploadAuthoringConfig(t, h, 1, 12, models.ContentTypeOther, []byte("// a different type")); w.Code != 200 {
		t.Fatalf("other type upload: %d %s", w.Code, w.Body.String())
	}
	if fs.mods[2].Slug == fs.mods[1].Slug {
		t.Fatal("another content type would mutate the existing catalog metadata")
	}
	prov := modpackProviderAt(t, dir)
	raw, err := prov.Get(context.Background(), first.StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, firstBytes) {
		t.Fatalf("first build's bytes changed: %q, %v", got, err)
	}
}

func TestRawUploadRejectsAnExistingTargetBeforeStorage(t *testing.T) {
	h, base, _ := newPackCleanup(t)
	fs := &authoringCaptureStore{packCleanupStore: base, existing: []models.BuildContentEntry{{Modversion: models.Modversion{TargetPath: "kubejs/server_scripts/same.js"}}}}
	h.state.Store = fs
	if w := uploadAuthoringConfig(t, h, 1, 10, models.ContentTypeConfig, []byte("// duplicate")); w.Code != 409 {
		t.Fatalf("duplicate upload: %d %s", w.Code, w.Body.String())
	}
	if fs.savedVersion != nil || len(fs.mods) != 0 {
		t.Fatal("duplicate request mutated catalog content")
	}
}
