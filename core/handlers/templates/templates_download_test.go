package templates

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

func (f *fakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	return f.billing[userID], nil
}

func TestDownloadName(t *testing.T) {
	for _, c := range []struct {
		name, want string
	}{
		{"My Pack", "My_Pack.zip"},
		{`a"b\r\nc`, "a_b_r_nc.zip"},
		{"../../etc", "etc.zip"},
		{"Ünï", "n.zip"},
		{"日本", "template-7.zip"},
		{"", "template-7.zip"},
	} {
		if got := downloadName(7, c.name); got != c.want {
			t.Errorf("downloadName(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDownloadStreamsAZip(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	if err := h.d.CoreStorage.TemplateWrite(context.Background(), tpl, "a/b.txt", strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.Download(rec, withID(asUser(httptest.NewRequest("GET", "/x", nil), ownerID, false), tpl.ID))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Disposition") != `attachment; filename="base.zip"` {
		t.Fatalf("got %d, %q", rec.Code, rec.Header().Get("Content-Disposition"))
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil || len(zr.File) != 1 || zr.File[0].Name != "a/b.txt" {
		t.Fatalf("zip: %v, %v", err, zr)
	}
	// A stranger cannot probe it.
	rec = httptest.NewRecorder()
	h.Download(rec, withID(asUser(httptest.NewRequest("GET", "/x", nil), strangerID, false), tpl.ID))
	if rec.Code != http.StatusNotFound {
		t.Errorf("stranger got %d, want 404", rec.Code)
	}
}

func TestDownloadStorageFailureIsGeneric(t *testing.T) {
	h, st := newTestHandler(t, map[string]string{"core_storage_backend": "", "core_storage_path": "", "core_storage_path_confirmed": ""})
	tpl := seedTemplate(t, st, ownerID, "personal")
	rec := httptest.NewRecorder()
	h.Download(rec, withID(asUser(httptest.NewRequest("GET", "/x", nil), ownerID, false), tpl.ID))
	if rec.Code != http.StatusBadGateway || rec.Header().Get("Content-Disposition") != "" {
		t.Errorf("got %d %q", rec.Code, rec.Header().Get("Content-Disposition"))
	}
}

func TestSuspendedOwnerKeepsReadDownloadDeleteButCannotWrite(t *testing.T) {
	for _, c := range []struct {
		name string
		b    store.UserBilling
	}{
		{"suspended", store.UserBilling{UserID: ownerID, Status: "suspended"}},
		{"admin hold", store.UserBilling{UserID: ownerID, Status: "suspended", AdminHold: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, st := newTestHandler(t, nil)
			tpl := seedTemplate(t, st, ownerID, "personal")
			if err := h.d.CoreStorage.TemplateWrite(context.Background(), tpl, "keep.txt", strings.NewReader("x"), 1); err != nil {
				t.Fatal(err)
			}
			b := c.b
			st.billing = map[string]*store.UserBilling{ownerID: &b}
			do := func(fn http.HandlerFunc, method, body string, admin bool) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				r := httptest.NewRequest(method, "/x", strings.NewReader(body))
				fn(rec, withID(asUser(r, ownerID, admin), tpl.ID))
				return rec
			}
			for name, fn := range map[string]http.HandlerFunc{
				"create":  h.Create,
				"upload":  h.Upload,
				"capture": h.Capture,
				"add":     h.AddFiles,
				"text":    h.PutText,
				"patch":   h.Update,
			} {
				if rec := do(fn, "POST", `{"name":"n","kind":"server"}`, false); rec.Code != http.StatusForbidden {
					t.Errorf("%s: got %d (%s), want 403", name, rec.Code, rec.Body)
				}
			}
			if len(st.templates) != 1 || len(storedFiles(t, h, tpl)) != 1 {
				t.Fatalf("a refused write changed something: %d rows, %v", len(st.templates), storedFiles(t, h, tpl))
			}
			// An admin panel session passes the gate (and then fails on its own body).
			if rec := do(h.PutText, "PUT", `{"path":"new.txt","content":"y"}`, true); rec.Code != http.StatusOK {
				t.Errorf("admin session: got %d (%s), want 200", rec.Code, rec.Body)
			}
			for name, fn := range map[string]http.HandlerFunc{"get": h.Get, "download": h.Download, "files": h.Files} {
				if rec := do(fn, "GET", "", false); rec.Code != http.StatusOK {
					t.Errorf("%s: got %d (%s), want 200", name, rec.Code, rec.Body)
				}
			}
			if rec := do(h.Delete, "DELETE", "", false); rec.Code != http.StatusOK || len(st.templates) != 0 {
				t.Errorf("delete: got %d, %d rows left", rec.Code, len(st.templates))
			}
		})
	}
}

func TestDownloadAbortsWhenTheZipFailsMidStream(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	old := templateZipTo
	defer func() { templateZipTo = old }()
	templateZipTo = func(_ context.Context, _ Deps, _ *models.Template, w io.Writer) error {
		_, _ = w.Write([]byte("PK"))
		return errors.New("storage went away")
	}
	rec := httptest.NewRecorder()
	r := withID(asUser(httptest.NewRequest("GET", "/x", nil), ownerID, false), tpl.ID)
	var got interface{}
	func() {
		defer func() { got = recover() }()
		h.Download(rec, r)
	}()
	if got != http.ErrAbortHandler {
		t.Errorf("recovered %v, want http.ErrAbortHandler", got)
	}
}

func TestEditSharerCannotWriteToASuspendedOwnersTemplate(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	st.shares = map[int]map[string]string{tpl.ID: {sharerID: "edit"}}
	st.billing = map[string]*store.UserBilling{ownerID: {UserID: ownerID, Status: "suspended"}}
	st.users["sharer"] = &models.User{ID: sharerID, Username: "sharer"}
	// Control: the same write by the sharer is allowed while the owner is fine.
	put := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", "/x", strings.NewReader(`{"path":"a.txt","content":"y"}`))
		h.PutText(rec, withID(asUser(r, sharerID, false), tpl.ID))
		return rec
	}
	if rec := put(); rec.Code != http.StatusForbidden {
		t.Errorf("sharer on suspended owner: got %d (%s), want 403", rec.Code, rec.Body)
	}
	delete(st.billing, ownerID)
	if rec := put(); rec.Code != http.StatusOK {
		t.Errorf("control, owner not suspended: got %d (%s), want 200", rec.Code, rec.Body)
	}
}

// List and Get carry the refusal a write would get, so the panel can say up
// front that the template is read-only while the owner is suspended.
func TestTemplateItemsCarryTheWriteRefusal(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	get := func() string {
		rec := httptest.NewRecorder()
		h.Get(rec, withID(asUser(httptest.NewRequest("GET", "/x", nil), ownerID, false), tpl.ID))
		var it struct {
			WriteRefusal string `json:"writeRefusal"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&it); err != nil {
			t.Fatal(err)
		}
		return it.WriteRefusal
	}
	list := func() string {
		rec := httptest.NewRecorder()
		h.List(rec, asUser(httptest.NewRequest("GET", "/x", nil), ownerID, false))
		var body struct {
			Templates []struct {
				WriteRefusal string `json:"writeRefusal"`
			} `json:"templates"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil || len(body.Templates) != 1 {
			t.Fatalf("list: %v %+v", err, body)
		}
		return body.Templates[0].WriteRefusal
	}
	if g, l := get(), list(); g != "" || l != "" {
		t.Fatalf("owner in good standing: %q / %q, want none", g, l)
	}
	st.billing = map[string]*store.UserBilling{ownerID: {UserID: ownerID, Status: "suspended"}}
	if g, l := get(), list(); !strings.Contains(g, "suspended") || g != l {
		t.Fatalf("suspended owner: get %q, list %q", g, l)
	}
}

// The download lists the Modrinth references in dylaris-modrinth.json at the
// zip root, and never contains their jars.
func TestDownloadCarriesTheModrinthManifest(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	if err := h.d.CoreStorage.TemplateWrite(context.Background(), tpl, "config/a.yml", strings.NewReader("x: 1"), 4); err != nil {
		t.Fatal(err)
	}
	// A stored file of the same name is replaced by the generated one.
	if err := h.d.CoreStorage.TemplateWrite(context.Background(), tpl, modrinthManifestName, strings.NewReader("stale"), 5); err != nil {
		t.Fatal(err)
	}
	ref := &models.TemplateModrinthRef{TemplateID: tpl.ID, Path: "plugins/LuckPerms.jar", ProjectID: "lp", VersionID: "v1",
		FileName: "LuckPerms.jar", URL: "https://cdn.modrinth.com/data/lp/LuckPerms.jar", Sha1: "s1", Sha512: "s512", SizeBytes: 9}
	if err := st.UpsertTemplateModrinth(ref); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.Download(rec, withID(asUser(httptest.NewRequest("GET", "/x", nil), ownerID, false), tpl.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var manifest []byte
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Name == modrinthManifestName {
			rc, _ := f.Open()
			manifest, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	if len(names) != 2 {
		t.Fatalf("entries %v", names)
	}
	var got struct {
		Version int                    `json:"version"`
		Files   []modrinthManifestFile `json:"files"`
	}
	if err := json.Unmarshal(manifest, &got); err != nil {
		t.Fatalf("manifest %q: %v", manifest, err)
	}
	want := modrinthManifestFile{"plugins/LuckPerms.jar", "lp", "v1", "https://cdn.modrinth.com/data/lp/LuckPerms.jar", "s512", "LuckPerms.jar"}
	if got.Version != 1 || len(got.Files) != 1 || got.Files[0] != want {
		t.Fatalf("manifest = %+v", got)
	}
}
