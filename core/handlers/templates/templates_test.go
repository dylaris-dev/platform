package templates

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/handlers/gate"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// fakeStore embeds store.Store (nil) so it satisfies the interface; only what
// the Templates handlers and the authz resolver touch is implemented.
type fakeStore struct {
	store.Store
	mu        sync.Mutex
	settings  map[string]string
	templates map[int]*models.Template
	nextID    int
	shares    map[int]map[string]string // template id -> user id -> level
	users     map[string]*models.User   // by username
	servers   map[int]*models.Server
	prefs     map[string]int // user id -> templates_storage_id
	storages  map[int]*models.BackupStorage
	billing   map[string]*store.UserBilling // by user id
	refs      []models.TemplateModrinthRef
	install   *models.SubServerInstall // nil: the paper record GetSubServerInstall returns
}

func (f *fakeStore) GetUserStoragePrefs(userID string) (*int, *int, error) {
	if id, ok := f.prefs[userID]; ok {
		return nil, &id, nil
	}
	return nil, nil, nil
}
func (f *fakeStore) GetBackupStorage(id int) (*models.BackupStorage, error) {
	return f.storages[id], nil
}
func (f *fakeStore) GetUserByID(id string) (*models.User, error) {
	for _, u := range f.users {
		if u.ID == id {
			return u, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (f *fakeStore) GetSetting(key string) (string, error) { return f.settings[key], nil }
func (f *fakeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}
func (f *fakeStore) GetUserByUsername(name string) (*models.User, error) {
	if u, ok := f.users[name]; ok {
		return u, nil
	}
	return nil, sql.ErrNoRows
}
func (f *fakeStore) CreateTemplate(t *models.Template) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	t.ID = f.nextID
	c := *t
	f.templates[t.ID] = &c
	return nil
}
func (f *fakeStore) GetTemplate(id int) (*models.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.templates[id]; ok {
		c := *t
		return &c, nil
	}
	return nil, nil
}
func (f *fakeStore) UpdateTemplate(t *models.Template) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *t
	f.templates[t.ID] = &c
	return nil
}
func (f *fakeStore) DeleteTemplate(id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.templates, id)
	return nil
}
func (f *fakeStore) SetTemplateSize(id int, size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.templates[id]; ok {
		t.SizeBytes = size
	}
	return nil
}
func (f *fakeStore) ListTemplatesVisibleTo(userID string, includeAdmin, includePlatform bool) ([]models.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []models.Template
	for _, t := range f.templates {
		if t.OwnerID == userID || f.shares[t.ID][userID] != "" ||
			(t.Visibility == "admin" && includeAdmin) || (t.Visibility == "platform" && includePlatform) {
			out = append(out, *t)
		}
	}
	return out, nil
}
func (f *fakeStore) TemplateShareLevel(id int, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shares[id][userID], nil
}
func (f *fakeStore) UpsertTemplateShare(id int, userID, level string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shares[id] == nil {
		f.shares[id] = map[string]string{}
	}
	f.shares[id][userID] = level
	return nil
}

const (
	ownerID    = "11111111-1111-1111-1111-111111111111"
	strangerID = "22222222-2222-2222-2222-222222222222"
	sharerID   = "33333333-3333-3333-3333-333333333333"
)

// storageRoot is a temp dir the Core storage path backend accepts: that check
// is Linux-explicit (must start with "/"), so the drive letter is dropped on
// Windows, where the rooted path still resolves on the current drive.
func storageRoot(t *testing.T) string {
	d := t.TempDir()
	return filepath.ToSlash(strings.TrimPrefix(d, filepath.VolumeName(d)))
}

func newTestHandler(t *testing.T, settings map[string]string) (*TemplatesHandler, *fakeStore) {
	t.Helper()
	all := map[string]string{
		services.TemplatesSettingKey:  "all",
		"core_storage_backend":        "path",
		"core_storage_path":           storageRoot(t),
		"core_storage_path_confirmed": "true",
	}
	for k, v := range settings {
		all[k] = v
	}
	st := &fakeStore{
		settings:  all,
		templates: map[int]*models.Template{},
		shares:    map[int]map[string]string{},
		users: map[string]*models.User{
			"owner":    {ID: ownerID, Username: "owner"},
			"stranger": {ID: strangerID, Username: "stranger"},
		},
	}
	state := withCoreStorage(&handlers.AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Authz: authz.NewResolver(st)})
	stubModrinth(t, nil, nil) // no test reaches the real Modrinth
	return NewTemplatesHandler(state), st
}

func asUser(r *http.Request, userID string, admin bool) *http.Request {
	ctx := context.WithValue(r.Context(), "userID", userID)
	ctx = context.WithValue(ctx, "isAdmin", admin)
	return r.WithContext(ctx)
}

func withID(r *http.Request, id int) *http.Request {
	return mux.SetURLVars(r, map[string]string{"id": strconv.Itoa(id)})
}

func seedTemplate(t *testing.T, st *fakeStore, owner, visibility string) *models.Template {
	t.Helper()
	tpl := &models.Template{OwnerID: owner, Name: "base", Kind: "server", Visibility: visibility}
	if err := st.CreateTemplate(tpl); err != nil {
		t.Fatal(err)
	}
	return tpl
}

type zipEntry struct {
	name    string
	body    []byte
	symlink bool
}

func makeZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.symlink {
			h.SetMode(os.ModeSymlink | 0o777)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uploadRequest(t *testing.T, meta string, zipBytes []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("meta", meta); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", "template.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(zipBytes); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/templates/upload", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

func storedFiles(t *testing.T, h *TemplatesHandler, tpl *models.Template) []string {
	t.Helper()
	files, err := h.state.CoreStorage.TemplateList(context.Background(), tpl)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	return names
}

func TestStrangerGetsNotFound(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	for _, id := range []int{tpl.ID, 999} {
		rec := httptest.NewRecorder()
		h.Get(rec, withID(asUser(httptest.NewRequest("GET", "/api/templates/x", nil), strangerID, false), id))
		if rec.Code != http.StatusNotFound {
			t.Errorf("template %d: stranger got %d, want 404", id, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.Get(rec, withID(asUser(httptest.NewRequest("GET", "/api/templates/x", nil), ownerID, false), tpl.ID))
	if rec.Code != http.StatusOK {
		t.Errorf("owner got %d, want 200", rec.Code)
	}
}

func TestUseSharerCannotWriteText(t *testing.T) {
	tests := []struct {
		level string
		want  int
	}{{"use", http.StatusForbidden}, {"edit", http.StatusOK}}
	for _, tc := range tests {
		h, st := newTestHandler(t, nil)
		tpl := seedTemplate(t, st, ownerID, "personal")
		st.UpsertTemplateShare(tpl.ID, sharerID, tc.level)
		body := `{"path":"server.properties","content":"motd=hi"}`
		rec := httptest.NewRecorder()
		h.PutText(rec, withID(asUser(httptest.NewRequest("PUT", "/x", strings.NewReader(body)), sharerID, false), tpl.ID))
		if rec.Code != tc.want {
			t.Errorf("%s sharer: got %d (%s), want %d", tc.level, rec.Code, rec.Body, tc.want)
		}
	}
}

func TestCreateBeyondMaxPerUser(t *testing.T) {
	h, st := newTestHandler(t, map[string]string{services.TemplatesMaxPerUserKey: "1"})
	body := `{"name":"one","kind":"server","visibility":"personal"}`
	codes := []int{http.StatusCreated, http.StatusConflict}
	for i, want := range codes {
		rec := httptest.NewRecorder()
		h.Create(rec, asUser(httptest.NewRequest("POST", "/api/templates", strings.NewReader(body)), ownerID, false))
		if rec.Code != want {
			t.Fatalf("create %d: got %d (%s), want %d", i+1, rec.Code, rec.Body, want)
		}
		if want == http.StatusConflict && !strings.Contains(rec.Body.String(), `"templates_limit"`) {
			t.Errorf("409 body %s lacks code templates_limit", rec.Body)
		}
	}
	if len(st.templates) != 1 {
		t.Errorf("store holds %d templates, want 1", len(st.templates))
	}
}

func TestUploadSkipsBadEntries(t *testing.T) {
	h, st := newTestHandler(t, nil)
	nestedJar := makeZip(t, []zipEntry{{name: "a.jar", body: []byte("jar")}})
	nestedOK := makeZip(t, []zipEntry{{name: "config/a.yml", body: []byte("b: 2")}})
	z := makeZip(t, []zipEntry{
		{name: "../evil", body: []byte("x")},
		{name: "ok.yml", body: []byte("a: 1")},
		{name: "bundle.zip", body: nestedJar},
		{name: "datapacks/ok.zip", body: nestedOK},
		{name: "plugins/Thing.JAR", body: []byte("jar")},
		{name: "link", body: []byte("/etc/passwd"), symlink: true},
	})
	rec := httptest.NewRecorder()
	h.Upload(rec, asUser(uploadRequest(t, `{"name":"up","kind":"server"}`, z), ownerID, false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d (%s), want 201", rec.Code, rec.Body)
	}
	var resp struct {
		Template models.Template `json:"template"`
		Skipped  []Skipped       `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	skipped := map[string]bool{}
	for _, s := range resp.Skipped {
		skipped[s.Path] = true
		if s.Reason == "" {
			t.Errorf("skipped %q has no reason", s.Path)
		}
	}
	for _, want := range []string{"../evil", "plugins/Thing.JAR", "link", "bundle.zip"} {
		if !skipped[want] {
			t.Errorf("skipped %v does not name %q", resp.Skipped, want)
		}
	}
	tpl, _ := st.GetTemplate(resp.Template.ID)
	if tpl == nil {
		t.Fatal("template row missing")
	}
	if got := strings.Join(storedFiles(t, h, tpl), ","); got != "datapacks/ok.zip,ok.yml" {
		t.Errorf("stored %v, want datapacks/ok.zip and ok.yml", got)
	}
	if want := int64(len("a: 1") + len(nestedOK)); tpl.SizeBytes != want {
		t.Errorf("size_bytes = %d, want %d", tpl.SizeBytes, want)
	}
}

func TestUploadOverSizeLeavesNothing(t *testing.T) {
	h, st := newTestHandler(t, map[string]string{services.TemplatesMaxSizeMBKey: "1"})
	// Zeros compress to almost nothing, so the body passes any transport cap and
	// only the running total during extraction can stop it.
	z := makeZip(t, []zipEntry{
		{name: "a.yml", body: []byte("first")},
		{name: "world/region.mca", body: make([]byte, 2<<20)},
	})
	rec := httptest.NewRecorder()
	h.Upload(rec, asUser(uploadRequest(t, `{"name":"big","kind":"server"}`, z), ownerID, false))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d (%s), want 413", rec.Code, rec.Body)
	}
	if len(st.templates) != 0 {
		t.Errorf("store still holds %d templates", len(st.templates))
	}
	// The row is gone, so ask storage directly for the id it had.
	ghost := &models.Template{ID: st.nextID, OwnerID: ownerID}
	if got := storedFiles(t, h, ghost); len(got) != 0 {
		t.Errorf("storage still holds %v", got)
	}
}

func TestAudienceOffRefusesUsers(t *testing.T) {
	h, _ := newTestHandler(t, map[string]string{services.TemplatesSettingKey: "off"})
	for _, admin := range []bool{false, true} {
		rec := httptest.NewRecorder()
		gate.Gates{Store: h.state.Store, FeatureFlags: h.state.FeatureFlags}.RequireTemplatesAudience(h.List)(rec, asUser(httptest.NewRequest("GET", "/api/templates", nil), ownerID, admin))
		want := http.StatusForbidden
		if admin {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Errorf("admin=%v: got %d, want %d", admin, rec.Code, want)
		}
	}
}

// TestEveryTemplatesRouteIsGated pins the wiring the access rules rely on:
// TemplateCanEdit/CanShare do not apply the audience, the route gate does.
func TestEveryTemplatesRouteIsGated(t *testing.T) {
	src, err := os.ReadFile("../../routes.go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, "templatesHandler.") || !strings.Contains(line, "HandleFunc(") {
			continue
		}
		n++
		if !strings.Contains(line, "AuthMiddleware(") || !strings.Contains(line, "RequireTemplatesAudience(") {
			t.Errorf("route not behind AuthMiddleware + RequireTemplatesAudience: %s", strings.TrimSpace(line))
		}
		if strings.Contains(line, "{id}") || regexp.MustCompile(`\{id:[^}]*\}`).FindString(line) != "" && !strings.Contains(line, "{id:[0-9]+}") {
			t.Errorf("template id route without {id:[0-9]+}: %s", strings.TrimSpace(line))
		}
	}
	if n < 15 {
		t.Errorf("found %d templates routes in routes.go, want at least 15", n)
	}
}

func TestShareWithYourselfIsRefused(t *testing.T) {
	tests := []struct {
		username string
		want     int
	}{{"owner", http.StatusBadRequest}, {"nobody", http.StatusBadRequest}, {"stranger", http.StatusOK}}
	for _, tc := range tests {
		h, st := newTestHandler(t, nil)
		tpl := seedTemplate(t, st, ownerID, "personal")
		body := `{"username":"` + tc.username + `","level":"use"}`
		rec := httptest.NewRecorder()
		h.PutShare(rec, withID(asUser(httptest.NewRequest("PUT", "/x", strings.NewReader(body)), ownerID, false), tpl.ID))
		if rec.Code != tc.want {
			t.Errorf("share with %s: got %d (%s), want %d", tc.username, rec.Code, rec.Body, tc.want)
		}
	}
}

func (f *fakeStore) GetStorageConnection(id int) (*models.StorageConnection, error) {
	return nil, errors.New("storage connection unavailable")
}

// untouchedBody fails the test when the handler reads the request body.
type untouchedBody struct{ t *testing.T }

func (b untouchedBody) Read([]byte) (int, error) {
	b.t.Error("the upload body was read although the caller may not create a template")
	return 0, io.EOF
}

func TestUploadRefusedBeforeTheBodyIsRead(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]string
		want     int
	}{
		{"at MaxPerUser", map[string]string{services.TemplatesMaxPerUserKey: "1"}, http.StatusConflict},
		{"role required", map[string]string{services.TemplatesCreateRequiresRoleKey: "true"}, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, st := newTestHandler(t, tc.settings)
			seedTemplate(t, st, ownerID, "personal")
			r := httptest.NewRequest(http.MethodPost, "/api/templates/upload", untouchedBody{t})
			r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
			rec := httptest.NewRecorder()
			h.Upload(rec, asUser(r, ownerID, false))
			if rec.Code != tc.want {
				t.Errorf("got %d (%s), want %d", rec.Code, rec.Body, tc.want)
			}
		})
	}
}

func TestOwnerCannotPromoteToPlatform(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	rec := httptest.NewRecorder()
	h.Update(rec, withID(asUser(httptest.NewRequest("PATCH", "/x", strings.NewReader(`{"visibility":"platform"}`)), ownerID, false), tpl.ID))
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d (%s), want 403", rec.Code, rec.Body)
	}
	if got, _ := st.GetTemplate(tpl.ID); got.Visibility != "personal" {
		t.Errorf("visibility changed to %q", got.Visibility)
	}
}

func TestEditSharerCannotDelete(t *testing.T) {
	tests := []struct {
		user string
		want int
	}{{sharerID, http.StatusForbidden}, {ownerID, http.StatusOK}}
	for _, tc := range tests {
		h, st := newTestHandler(t, nil)
		tpl := seedTemplate(t, st, ownerID, "personal")
		st.UpsertTemplateShare(tpl.ID, sharerID, "edit")
		rec := httptest.NewRecorder()
		h.Delete(rec, withID(asUser(httptest.NewRequest("DELETE", "/x", nil), tc.user, false), tpl.ID))
		if rec.Code != tc.want {
			t.Errorf("%s: got %d (%s), want %d", tc.user, rec.Code, rec.Body, tc.want)
		}
		if _, gone := st.templates[tpl.ID]; gone == (tc.want == http.StatusOK) {
			t.Errorf("%s: row present = %v after %d", tc.user, gone, rec.Code)
		}
	}
}

// A storage connection that cannot be built is refused before the row exists,
// so there is no row for discard to keep.
func TestUploadWithUnusableStorageConnectionIsRefused(t *testing.T) {
	h, st := newTestHandler(t, map[string]string{services.TemplatesStorageConnectionKey: "5"})
	z := makeZip(t, []zipEntry{{name: "ok.yml", body: []byte("a: 1")}})
	rec := httptest.NewRecorder()
	h.Upload(rec, asUser(uploadRequest(t, `{"name":"up","kind":"server"}`, z), ownerID, false))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d (%s), want 503", rec.Code, rec.Body)
	}
	if len(st.templates) != 0 {
		t.Errorf("store holds %d templates, want none", len(st.templates))
	}
}

func TestUploadOverEntryCapLeavesNothing(t *testing.T) {
	h, st := newTestHandler(t, nil)
	entries := make([]zipEntry, maxZipEntries+1)
	for i := range entries {
		entries[i] = zipEntry{name: "f/" + strconv.Itoa(i)}
	}
	rec := httptest.NewRecorder()
	h.Upload(rec, asUser(uploadRequest(t, `{"name":"many","kind":"server"}`, makeZip(t, entries)), ownerID, false))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "more than 20000") {
		t.Fatalf("got %d (%s), want 400 naming the entry cap", rec.Code, rec.Body)
	}
	if len(st.templates) != 0 {
		t.Errorf("store still holds %d templates", len(st.templates))
	}
	ghost := &models.Template{ID: st.nextID, OwnerID: ownerID}
	if got := storedFiles(t, h, ghost); len(got) != 0 {
		t.Errorf("storage still holds %d files", len(got))
	}
}

func TestSkippedListIsCapped(t *testing.T) {
	tests := []struct {
		bad       int
		wantLen   int
		wantTotal string
	}{
		{maxSkippedListed, maxSkippedListed, ""},
		{maxSkippedListed + 7, maxSkippedListed + 1, "... and 7 more"},
	}
	for _, tc := range tests {
		h, _ := newTestHandler(t, nil)
		entries := []zipEntry{{name: "ok.yml", body: []byte("a: 1")}}
		for i := 0; i < tc.bad; i++ {
			entries = append(entries, zipEntry{name: "../evil" + strconv.Itoa(i)})
		}
		rec := httptest.NewRecorder()
		h.Upload(rec, asUser(uploadRequest(t, `{"name":"up","kind":"server"}`, makeZip(t, entries)), ownerID, false))
		if rec.Code != http.StatusCreated {
			t.Fatalf("%d bad: got %d (%s), want 201", tc.bad, rec.Code, rec.Body)
		}
		var resp struct {
			Skipped []Skipped `json:"skipped"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Skipped) != tc.wantLen {
			t.Fatalf("%d bad: %d skipped entries, want %d", tc.bad, len(resp.Skipped), tc.wantLen)
		}
		if last := resp.Skipped[len(resp.Skipped)-1].Path; tc.wantTotal != "" && last != tc.wantTotal {
			t.Errorf("%d bad: last entry %q, want %q", tc.bad, last, tc.wantTotal)
		}
	}
}

// No Core storage configured: nothing may create a row, and a row left by the
// old behaviour can still be deleted.
func TestCreateWithoutStorageIsRefusedAndLeavesNoRow(t *testing.T) {
	unset := map[string]string{"core_storage_backend": "", "core_storage_path": "", "core_storage_path_confirmed": ""}
	z := makeZip(t, []zipEntry{{name: "ok.yml", body: []byte("a: 1")}})
	tests := []struct {
		name string
		call func(h *TemplatesHandler, rec *httptest.ResponseRecorder)
	}{
		{"create", func(h *TemplatesHandler, rec *httptest.ResponseRecorder) {
			r := httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"n","kind":"server"}`))
			h.Create(rec, asUser(r, ownerID, false))
		}},
		{"upload", func(h *TemplatesHandler, rec *httptest.ResponseRecorder) {
			h.Upload(rec, asUser(uploadRequest(t, `{"name":"n","kind":"server"}`, z), ownerID, false))
		}},
		{"capture", func(h *TemplatesHandler, rec *httptest.ResponseRecorder) {
			r := httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"n","kind":"server","serverId":7,"subServer":"main","selected":["a"]}`))
			h.Capture(rec, asUser(r, ownerID, false))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, st := newTestHandler(t, unset)
			st.servers = map[int]*models.Server{7: {ID: 7, OwnerID: ownerID, UUID: "u-7"}}
			rec := httptest.NewRecorder()
			tc.call(h, rec)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("got %d (%s), want 503", rec.Code, rec.Body)
			}
			if len(st.templates) != 0 {
				t.Errorf("store holds %d templates, want none", len(st.templates))
			}
		})
	}
}

func TestDeleteOnNeverConfiguredStorageRemovesTheRow(t *testing.T) {
	h, st := newTestHandler(t, map[string]string{"core_storage_backend": "", "core_storage_path": "", "core_storage_path_confirmed": ""})
	tpl := seedTemplate(t, st, ownerID, "personal")
	rec := httptest.NewRecorder()
	h.Delete(rec, withID(asUser(httptest.NewRequest("DELETE", "/x", nil), ownerID, false), tpl.ID))
	if rec.Code != http.StatusOK || len(st.templates) != 0 {
		t.Errorf("got %d (%s), %d rows left, want 200 and none", rec.Code, rec.Body, len(st.templates))
	}
}
