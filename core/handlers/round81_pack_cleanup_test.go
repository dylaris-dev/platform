package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/store"
)

const cleanupOwner = "11111111-1111-4111-8111-111111111111"

// packCleanupStore holds one pack with two builds on a local modpack backend.
type packCleanupStore struct {
	store.Store
	settings map[string]string
	pack     *models.Pack
	builds   []models.PackBuild
	deleted  []string
	failMod  bool
	failMV   bool
}

func (f *packCleanupStore) CreateModversion(*models.Modversion) (int, error) {
	if f.failMV {
		return 0, errors.New("db down")
	}
	return 1, nil
}

func (f *packCleanupStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *packCleanupStore) GetPack(int) (*models.Pack, error)   { return f.pack, nil }
func (f *packCleanupStore) ListPacksByOwner(string) ([]models.Pack, error) {
	return []models.Pack{*f.pack}, nil
}
func (f *packCleanupStore) ListPackBuilds(int) ([]models.PackBuild, error) { return f.builds, nil }
func (f *packCleanupStore) GetPackBuild(id int) (*models.PackBuild, error) {
	for i := range f.builds {
		if f.builds[i].ID == id {
			b := f.builds[i]
			return &b, nil
		}
	}
	return nil, errors.New("no build")
}
func (f *packCleanupStore) DeletePack(int, string) error {
	f.deleted = append(f.deleted, "pack")
	return nil
}
func (f *packCleanupStore) DeletePackBuild(int, int) error {
	f.deleted = append(f.deleted, "build")
	return nil
}
func (f *packCleanupStore) UpdatePackBuild(*models.PackBuild) error { return nil }
func (f *packCleanupStore) UpsertMod(*models.Mod) (int, error) {
	if f.failMod {
		return 0, errors.New("db down")
	}
	return 1, nil
}
func (f *packCleanupStore) CountModversionsByStorageKey(string) (int, error) { return 0, nil }

func newPackCleanup(t *testing.T) (*PacksHandler, *packCleanupStore, string) {
	t.Helper()
	dir := t.TempDir()
	paths, _ := json.Marshal([]string{dir})
	fs := &packCleanupStore{
		settings: map[string]string{"modpack_storage_provider": "local", "modpack_storage_paths": string(paths)},
		pack:     &models.Pack{ID: 1, OwnerID: cleanupOwner, InternalSlug: "pack"},
		builds:   []models.PackBuild{{ID: 10, PackID: 1, VersionString: "1.0"}, {ID: 11, PackID: 1, VersionString: "2.0"}},
	}
	h := &PacksHandler{state: &AppState{Store: fs, ClusterSecret: "test-cluster-secret-0123456789"}}
	return h, fs, dir
}

// seedBuild writes what a build's directory holds after an install and a
// share: the .mrpack, a server pack and the marker naming it.
func seedBuild(t *testing.T, h *PacksHandler, root string, b *models.PackBuild) string {
	t.Helper()
	d := h.state.packBuildDirs(h.state.Store.(*packCleanupStore).pack, b)[0]
	files := map[string]string{
		d + "pack.mrpack":     "mrpack",
		d + "server-abc.zip":  "zip",
		d + "server.last":     d + "server-abc.zip",
		d + "server-prev.zip": "an older render the marker no longer names",
	}
	for k, v := range files {
		full := filepath.Join(root, filepath.FromSlash(k))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func exists(root, key string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(key)))
	return err == nil
}

func packReq(method string, vars map[string]string, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	r = r.WithContext(context.WithValue(r.Context(), "userID", cleanupOwner))
	return mux.SetURLVars(r, vars)
}

func TestDeletingABuildRemovesItsStoredObjects(t *testing.T) {
	h, _, root := newPackCleanup(t)
	gone := seedBuild(t, h, root, &h.state.Store.(*packCleanupStore).builds[0])
	kept := seedBuild(t, h, root, &h.state.Store.(*packCleanupStore).builds[1])
	h.DeleteBuild(httptest.NewRecorder(), packReq("DELETE", map[string]string{"id": "1", "buildId": "10"}, ""))
	for _, k := range []string{"pack.mrpack", "server-abc.zip", "server.last"} {
		if exists(root, gone+k) {
			t.Errorf("%s survived its build", k)
		}
		if !exists(root, kept+k) {
			t.Errorf("the other build lost %s", k)
		}
	}
}

func TestDeletingAPackRemovesEveryBuildsObjects(t *testing.T) {
	h, fs, root := newPackCleanup(t)
	a := seedBuild(t, h, root, &fs.builds[0])
	b := seedBuild(t, h, root, &fs.builds[1])
	h.Delete(httptest.NewRecorder(), packReq("DELETE", map[string]string{"id": "1"}, ""))
	for _, d := range []string{a, b} {
		if exists(root, d+"pack.mrpack") || exists(root, d+"server-abc.zip") {
			t.Errorf("objects under %s survived the pack", d)
		}
	}
}

func TestDeletingAnAccountRemovesItsPackObjects(t *testing.T) {
	h, fs, root := newPackCleanup(t)
	d := seedBuild(t, h, root, &fs.builds[0])
	h.state.DropPackDirs(h.state.PackDirsOfUser(cleanupOwner))
	if exists(root, d+"pack.mrpack") {
		t.Error("the account's objects survived")
	}
}

// A rename moves the derived directory, which stranded the old one.
func TestRenamingABuildRemovesTheOldDirectory(t *testing.T) {
	h, fs, root := newPackCleanup(t)
	old := seedBuild(t, h, root, &fs.builds[0])
	h.UpdateBuild(httptest.NewRecorder(), packReq("PATCH", map[string]string{"id": "1", "buildId": "10"}, `{"versionString":"1.0"}`))
	if !exists(root, old+"pack.mrpack") {
		t.Fatal("an unchanged version lost its objects")
	}
	h.UpdateBuild(httptest.NewRecorder(), packReq("PATCH", map[string]string{"id": "1", "buildId": "10"}, `{"versionString":"1.1"}`))
	if exists(root, old+"pack.mrpack") {
		t.Error("the old directory survived the rename")
	}
}

// The marker is an object like any other: one naming a key outside its own
// directory must not make the cleanup delete that key.
func TestDropPackDirsIgnoresAMarkerPointingElsewhere(t *testing.T) {
	h, fs, root := newPackCleanup(t)
	d := seedBuild(t, h, root, &fs.builds[0])
	other := seedBuild(t, h, root, &fs.builds[1])
	os.WriteFile(filepath.Join(root, filepath.FromSlash(d+"server.last")), []byte(other+"server-abc.zip"), 0o644)
	h.state.DropPackDirs([]string{d})
	if !exists(root, other+"server-abc.zip") {
		t.Error("a marker deleted another build's server pack")
	}
}

// The object is stored before the rows; a failed row left it for good.
func TestAFailedUploadRemovesTheObjectItStored(t *testing.T) {
	for _, step := range []string{"mod", "version"} {
		t.Run(step, func(t *testing.T) { failedUploadLeavesNothing(t, step) })
	}
}

func failedUploadLeavesNothing(t *testing.T, step string) {
	h, fs, root := newPackCleanup(t)
	fs.failMod, fs.failMV = step == "mod", step == "version"
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "thing.jar")
	fw.Write([]byte("PK-not-really-a-jar"))
	mw.Close()
	r := packReq("POST", map[string]string{"id": "1", "buildId": "10"}, body.String())
	r.Header.Set("Content-Type", mw.FormDataContentType())
	rw := httptest.NewRecorder()
	h.UploadContent(rw, r)
	if rw.Code != http.StatusInternalServerError {
		t.Fatalf("status %d: %s", rw.Code, rw.Body)
	}
	var left []string
	filepath.Walk(filepath.Join(root, "packs"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			left = append(left, p)
		}
		return nil
	})
	if len(left) != 0 {
		t.Errorf("a failed upload left %v", left)
	}
}

// The admin delete: the account's packs cascade with the row, so their
// directories are read first and emptied after.
type userDeletePackStore struct {
	*userDeleteWarpStore
	pc         *packCleanupStore
	failDelete bool
}

func (f *userDeletePackStore) GetSetting(k string) (string, error) { return f.pc.GetSetting(k) }
func (f *userDeletePackStore) ListPacksByOwner(o string) ([]models.Pack, error) {
	return f.pc.ListPacksByOwner(o)
}
func (f *userDeletePackStore) ListPackBuilds(id int) ([]models.PackBuild, error) {
	return f.pc.ListPackBuilds(id)
}

func (f *userDeletePackStore) DeleteUser(id string) error {
	if f.failDelete {
		return errors.New("still referenced")
	}
	return nil
}

func TestDeleteUserRemovesThePackObjects(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "refused"}[fail], func(t *testing.T) { deleteUserPackObjects(t, fail) })
	}
}

// A refused delete keeps the account and its packs, so it must keep their
// objects too: that is the direction a cleanup cannot take back.
func deleteUserPackObjects(t *testing.T, fail bool) {
	h, fs, root := newPackCleanup(t)
	d := seedBuild(t, h, root, &fs.builds[0])
	st := &AppState{Store: &userDeletePackStore{userDeleteWarpStore: &userDeleteWarpStore{}, pc: fs, failDelete: fail}, ClusterSecret: h.state.ClusterSecret}
	r := httptest.NewRequest(http.MethodDelete, "/api/users/"+cleanupOwner, bytes.NewBufferString(`{"reauth":{"password":"`+testReauthPassword+`"}}`))
	ctx := context.WithValue(r.Context(), "userID", "admin-id")
	ctx = context.WithValue(ctx, "username", "admin")
	ctx = context.WithValue(ctx, "isAdmin", true)
	r = mux.SetURLVars(r.WithContext(ctx), map[string]string{"id": cleanupOwner})
	rec := httptest.NewRecorder()
	NewUserHandler(st).DeleteUser(rec, r)
	if fail {
		if rec.Code == http.StatusOK || !exists(root, d+"pack.mrpack") {
			t.Fatalf("a refused delete (%d) removed the account's pack objects", rec.Code)
		}
		return
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if exists(root, d+"pack.mrpack") {
		t.Error("the deleted account's pack objects survived")
	}
}
