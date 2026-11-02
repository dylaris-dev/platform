package templates

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
)

func (f *fakeStore) ListTemplateModrinth(templateID int) ([]models.TemplateModrinthRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []models.TemplateModrinthRef{}
	for _, r := range f.refs {
		if r.TemplateID == templateID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (f *fakeStore) UpsertTemplateModrinth(r *models.TemplateModrinthRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, old := range f.refs {
		if old.TemplateID == r.TemplateID && old.Path == r.Path {
			r.ID = old.ID
			f.refs[i] = *r
			return nil
		}
	}
	r.ID = len(f.refs) + 1
	f.refs = append(f.refs, *r)
	return nil
}

func (f *fakeStore) DeleteTemplateModrinthPath(templateID int, rel string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	keep := f.refs[:0]
	for _, r := range f.refs {
		if r.TemplateID == templateID && (r.Path == rel || strings.HasPrefix(r.Path, rel+"/")) {
			continue
		}
		keep = append(keep, r)
	}
	f.refs = keep
	return nil
}

func (f *fakeStore) DeleteTemplateModrinth(templateID, id int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.refs {
		if r.TemplateID == templateID && r.ID == id {
			f.refs = append(f.refs[:i], f.refs[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// stubModrinth stands in for Modrinth: a jar whose content is in known is on
// Modrinth as that version id, every other jar is not; a non-nil fail makes
// Modrinth unreachable. It records how many lookups were made.
func stubModrinth(t *testing.T, known map[string]string, fail error) *int {
	t.Helper()
	calls := new(int)
	lastJars = nil
	bySha1 := map[string]string{}
	for content, version := range known {
		j, _ := services.HashTemplateJar("", strings.NewReader(content))
		bySha1[j.Sha1] = version
	}
	prev := resolveJars
	t.Cleanup(func() { resolveJars = prev })
	prevFetch := fetchModrinthVersion
	t.Cleanup(func() { fetchModrinthVersion = prevFetch })
	fetchModrinthVersion = func(context.Context, string) (*services.ModrinthVersion, error) {
		return nil, errors.New("no test reaches the real Modrinth")
	}
	resolveJars = func(jars []services.TemplateJar) ([]models.TemplateModrinthRef, map[string]string) {
		*calls++
		lastJars = jars
		reasons := map[string]string{}
		var refs []models.TemplateModrinthRef
		for _, j := range jars {
			v, ok := bySha1[j.Sha1]
			switch {
			case fail != nil:
				reasons[j.Path] = services.TemplateJarUnreachable
			case !ok:
				reasons[j.Path] = services.TemplateJarNotOnModrinth
			default:
				refs = append(refs, models.TemplateModrinthRef{Path: j.Path, ProjectID: "p-" + v, VersionID: v,
					URL: "https://cdn.modrinth.com/data/x/" + v + ".jar", Sha1: j.Sha1, Sha512: j.Sha512, SizeBytes: j.Size})
			}
		}
		return refs, reasons
	}
	return calls
}

func extractZip(t *testing.T, h *TemplatesHandler, tpl *models.Template, entries []zipEntry, policy string) []Skipped {
	t.Helper()
	p := t.TempDir() + "/capture.zip"
	if err := os.WriteFile(p, makeZip(t, entries), 0o600); err != nil {
		t.Fatal(err)
	}
	skipped, err := extractTemplateZip(context.Background(), h.d, tpl, p, services.TemplateSettings{ContentPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return skipped
}

func reasonsByPath(skipped []Skipped) map[string]string {
	out := map[string]string{}
	for _, s := range skipped {
		out[s.Path] = s.Reason
	}
	return out
}

// A captured server under no-jars: the Modrinth plugin becomes a reference,
// the hand-made jar and the server jar are skipped, configs are stored, and
// the reference does not count toward the size.
func TestCaptureKeepsModrinthJarsAsReferences(t *testing.T) {
	h, st := newTestHandler(t, nil)
	calls := stubModrinth(t, map[string]string{"luckperms": "v-lp"}, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	skipped := extractZip(t, h, tpl, []zipEntry{
		{name: "server.jar", body: []byte("luckperms")},
		{name: "plugins/LuckPerms.jar", body: []byte("luckperms")},
		{name: "plugins/Handmade.jar", body: []byte("mine")},
		{name: "plugins/LuckPerms/config.yml", body: []byte("k: v")},
		// On Modrinth, but where no install can lay a reference: not looked up.
		{name: "libraries/net/LuckPerms.jar", body: []byte("luckperms")},
	}, services.TemplatePolicyNoJars)

	if *calls != 1 {
		t.Errorf("%d Modrinth lookups, want one per request", *calls)
	}
	if len(lastJars) != 2 {
		t.Errorf("looked up %d jars, want the two under plugins/", len(lastJars))
	}
	reasons := reasonsByPath(skipped)
	if reasons["plugins/Handmade.jar"] != services.TemplateJarNotOnModrinth || reasons["server.jar"] == "" ||
		reasons["libraries/net/LuckPerms.jar"] != "only jars in mods/ or plugins/ can be kept as Modrinth references" || len(reasons) != 3 {
		t.Errorf("skipped %v, want the hand-made jar (not on Modrinth), the server jar and the libraries jar", skipped)
	}
	if got := strings.Join(storedFiles(t, h, tpl), ","); got != "plugins/LuckPerms/config.yml" {
		t.Errorf("stored %q, want only the config", got)
	}
	refs, _ := st.ListTemplateModrinth(tpl.ID)
	if len(refs) != 1 || refs[0].Path != "plugins/LuckPerms.jar" || refs[0].VersionID != "v-lp" {
		t.Fatalf("refs = %+v, want the plugin", refs)
	}
	if got, _ := st.GetTemplate(tpl.ID); got.SizeBytes != int64(len("k: v")) {
		t.Errorf("size_bytes = %d: a reference must not count", got.SizeBytes)
	}
}

func TestCaptureWithModrinthDownStoresTheRest(t *testing.T) {
	h, st := newTestHandler(t, nil)
	stubModrinth(t, map[string]string{"luckperms": "v-lp"}, errors.New("down"))
	tpl := seedTemplate(t, st, ownerID, "personal")
	skipped := extractZip(t, h, tpl, []zipEntry{
		{name: "plugins/LuckPerms.jar", body: []byte("luckperms")},
		{name: "plugins/Other.jar", body: []byte("other")},
		{name: "server.properties", body: []byte("motd=hi")},
	}, services.TemplatePolicyNoJars)
	reasons := reasonsByPath(skipped)
	for _, p := range []string{"plugins/LuckPerms.jar", "plugins/Other.jar"} {
		if reasons[p] != services.TemplateJarUnreachable {
			t.Errorf("%s: reason %q, want unreachable", p, reasons[p])
		}
	}
	if got := strings.Join(storedFiles(t, h, tpl), ","); got != "server.properties" {
		t.Errorf("stored %q", got)
	}
	if refs, _ := st.ListTemplateModrinth(tpl.ID); len(refs) != 0 {
		t.Errorf("refs %+v from a failed lookup", refs)
	}
}

// Under uploads a jar is a stored file like any other and Modrinth is not asked.
func TestUploadsPolicyStoresJars(t *testing.T) {
	h, st := newTestHandler(t, nil)
	calls := stubModrinth(t, map[string]string{"luckperms": "v-lp"}, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	extractZip(t, h, tpl, []zipEntry{{name: "plugins/LuckPerms.jar", body: []byte("luckperms")}}, services.TemplatePolicyUploads)
	if *calls != 0 {
		t.Errorf("%d Modrinth lookups under uploads", *calls)
	}
	if got := strings.Join(storedFiles(t, h, tpl), ","); got != "plugins/LuckPerms.jar" {
		t.Errorf("stored %q", got)
	}
}

func addFilesRequest(t *testing.T, files map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for p, content := range files {
		mw.WriteField("path", p)
		fw, _ := mw.CreateFormFile("file", "f")
		fw.Write([]byte(content))
	}
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/x", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

// Added files take the same path as a capture, and the listing shows the
// reference; deleting its folder removes it.
func TestAddFilesReferenceListedAndDeleted(t *testing.T) {
	h, st := newTestHandler(t, nil)
	stubModrinth(t, map[string]string{"sodium": "v-so"}, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	rec := httptest.NewRecorder()
	h.AddFiles(rec, withID(asUser(addFilesRequest(t, map[string]string{"mods/sodium.jar": "sodium", "mods/mine.jar": "mine"}), ownerID, false), tpl.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	var added struct{ Skipped []Skipped }
	json.Unmarshal(rec.Body.Bytes(), &added)
	if r := reasonsByPath(added.Skipped); len(r) != 1 || r["mods/mine.jar"] != services.TemplateJarNotOnModrinth {
		t.Errorf("skipped %v, want mods/mine.jar", added.Skipped)
	}

	rec = httptest.NewRecorder()
	h.Files(rec, withID(asUser(httptest.NewRequest("GET", "/x", nil), ownerID, false), tpl.ID))
	var listed struct {
		Files []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			Modrinth *struct {
				ProjectID string `json:"projectId"`
				VersionID string `json:"versionId"`
			} `json:"modrinth"`
		} `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Files) != 1 || listed.Files[0].Name != "mods/sodium.jar" || listed.Files[0].Modrinth == nil ||
		listed.Files[0].Modrinth.VersionID != "v-so" || listed.Files[0].Size != int64(len("sodium")) {
		t.Fatalf("files = %s, want the sodium reference", rec.Body)
	}

	rec = httptest.NewRecorder()
	h.DeleteFile(rec, withID(asUser(httptest.NewRequest("DELETE", "/x?path=mods", nil), ownerID, false), tpl.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if refs, _ := st.ListTemplateModrinth(tpl.ID); len(refs) != 0 {
		t.Errorf("deleting the folder left %+v", refs)
	}
}

// lastJars is what the last stubbed lookup was asked about.
var lastJars []services.TemplateJar

// Hashed jar bytes count against a budget of the request size, so a zip of
// highly compressible jars cannot buy unbounded hashing: past it the jars are
// skipped unread, and the rest of the zip still lands.
func TestJarHashingIsBounded(t *testing.T) {
	h, st := newTestHandler(t, nil)
	stubModrinth(t, nil, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	limit := int64(1 << 20)
	entries := []zipEntry{{name: "server.properties", body: []byte("motd=hi")}}
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		entries = append(entries, zipEntry{name: "plugins/" + n + ".jar", body: make([]byte, 400<<10)})
	}
	p := t.TempDir() + "/bomb.zip"
	if err := os.WriteFile(p, makeZip(t, entries), 0o600); err != nil {
		t.Fatal(err)
	}
	skipped, err := extractTemplateZip(context.Background(), h.d, tpl, p,
		services.TemplateSettings{ContentPolicy: services.TemplatePolicyNoJars, MaxSizeBytes: &limit})
	if err != nil {
		t.Fatal(err)
	}
	if len(lastJars) != 2 {
		t.Errorf("%d jars hashed, want the 2 that fit in 1 MiB", len(lastJars))
	}
	budget := 0
	for _, s := range skipped {
		if s.Reason == jarBudgetReason {
			budget++
		}
	}
	if budget != 4 {
		t.Errorf("skipped %v, want 4 over the budget", skipped)
	}
	if got := strings.Join(storedFiles(t, h, tpl), ","); got != "server.properties" {
		t.Errorf("stored %q", got)
	}
}

// A renamed jar is caught by its bytes under no-jars; any other file is not.
func TestRenamedJarIsRefused(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	jar := makeZip(t, []zipEntry{{name: "META-INF/MANIFEST.MF", body: []byte("Manifest-Version: 1.0")}, {name: "com/x/Main.class", body: []byte{0xca, 0xfe}}})
	skipped := extractZip(t, h, tpl, []zipEntry{
		{name: "plugins/lib.dat", body: jar},
		{name: "world/level.dat", body: []byte("nbt")},
	}, services.TemplatePolicyNoJars)
	if r := reasonsByPath(skipped); len(r) != 1 || r["plugins/lib.dat"] == "" {
		t.Errorf("skipped %v, want plugins/lib.dat", skipped)
	}
	if got := strings.Join(storedFiles(t, h, tpl), ","); got != "world/level.dat" {
		t.Errorf("stored %q", got)
	}

	rec := httptest.NewRecorder()
	h.AddFiles(rec, withID(asUser(addFilesRequest(t, map[string]string{"mods/x.bin": string(jar)}), ownerID, false), tpl.ID))
	var added struct{ Skipped []Skipped }
	json.Unmarshal(rec.Body.Bytes(), &added)
	if rec.Code != http.StatusOK || len(added.Skipped) != 1 || added.Skipped[0].Path != "mods/x.bin" {
		t.Errorf("add-files: %d %s, want mods/x.bin skipped", rec.Code, rec.Body)
	}
}

// The last write at a path wins, in both directions.
func TestLastWriteAtAPathWins(t *testing.T) {
	ref := func(tplID int, p string) *models.TemplateModrinthRef {
		return &models.TemplateModrinthRef{TemplateID: tplID, Path: p, ProjectID: "p", VersionID: "v"}
	}

	t.Run("a stored file replaces a reference", func(t *testing.T) {
		h, st := newTestHandler(t, map[string]string{services.TemplatesContentPolicyKey: services.TemplatePolicyUploads})
		tpl := seedTemplate(t, st, ownerID, "personal")
		st.UpsertTemplateModrinth(ref(tpl.ID, "mods/x.jar"))
		st.UpsertTemplateModrinth(ref(tpl.ID, "config/a.yml"))
		st.UpsertTemplateModrinth(ref(tpl.ID, "mods/keep.jar"))
		rec := httptest.NewRecorder()
		h.AddFiles(rec, withID(asUser(addFilesRequest(t, map[string]string{"mods/x.jar": "own build"}), ownerID, false), tpl.ID))
		if rec.Code != http.StatusOK {
			t.Fatalf("add: %d %s", rec.Code, rec.Body)
		}
		rec = httptest.NewRecorder()
		h.PutText(rec, withID(asUser(httptest.NewRequest("PUT", "/x", strings.NewReader(`{"path":"config/a.yml","content":"k: v"}`)), ownerID, false), tpl.ID))
		if rec.Code != http.StatusOK {
			t.Fatalf("put: %d %s", rec.Code, rec.Body)
		}
		refs, _ := st.ListTemplateModrinth(tpl.ID)
		if len(refs) != 1 || refs[0].Path != "mods/keep.jar" {
			t.Errorf("refs %+v, want only mods/keep.jar", refs)
		}
	})

	t.Run("a reference replaces a stored file", func(t *testing.T) {
		h, st := newTestHandler(t, nil)
		stubModrinth(t, map[string]string{"sodium": "v-so"}, nil)
		tpl := seedTemplate(t, st, ownerID, "personal")
		if err := h.d.CoreStorage.TemplateWrite(context.Background(), tpl, "mods/sodium.jar", strings.NewReader("old copy"), 8); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.AddFiles(rec, withID(asUser(addFilesRequest(t, map[string]string{"mods/sodium.jar": "sodium"}), ownerID, false), tpl.ID))
		if rec.Code != http.StatusOK {
			t.Fatalf("add: %d %s", rec.Code, rec.Body)
		}
		if got := storedFiles(t, h, tpl); len(got) != 0 {
			t.Errorf("stored %v, want the copy gone", got)
		}
		if refs, _ := st.ListTemplateModrinth(tpl.ID); len(refs) != 1 || refs[0].Path != "mods/sodium.jar" {
			t.Errorf("refs %+v", refs)
		}
	})
}

// storedJar is a jar whose class is stored uncompressed, so the archive itself
// is size bytes while the outer zip deflates it to almost nothing.
func storedJar(t *testing.T, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "com/x/Main.class", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	w.Write(make([]byte, size))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Refused archives count against their own budget of the request size: past
// it the rest are skipped unread, and the request still succeeds.
func TestRefusedArchiveStagingIsBounded(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	limit := int64(1 << 20)
	entries := []zipEntry{{name: "server.properties", body: []byte("motd=hi")}}
	jar := storedJar(t, 400<<10)
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		entries = append(entries, zipEntry{name: "libs/" + n + ".dat", body: jar})
	}
	p := t.TempDir() + "/bomb.zip"
	if err := os.WriteFile(p, makeZip(t, entries), 0o600); err != nil {
		t.Fatal(err)
	}
	skipped, err := extractTemplateZip(context.Background(), h.d, tpl, p,
		services.TemplateSettings{ContentPolicy: services.TemplatePolicyNoJars, MaxSizeBytes: &limit})
	if err != nil {
		t.Fatal(err)
	}
	budget := 0
	for _, s := range skipped {
		if s.Reason == archiveBudgetReason {
			budget++
		}
	}
	if len(skipped) != 6 || budget != 4 {
		t.Errorf("skipped %v, want 2 inspected and refused, 4 over the budget", skipped)
	}
	if got := strings.Join(storedFiles(t, h, tpl), ","); got != "server.properties" {
		t.Errorf("stored %q", got)
	}
}

// Capture and add-files stop at the per-template cap: a jar at a path that
// already holds a reference replaces it, any other one past the cap is skipped.
func TestCapturedReferencesStopAtTheCap(t *testing.T) {
	if refCapReason != fmt.Sprintf("a template can hold at most %d Modrinth references", maxTemplateRefs) {
		t.Fatalf("reason %q does not match the cap", refCapReason)
	}
	h, st := newTestHandler(t, nil)
	stubModrinth(t, map[string]string{"a": "va", "b": "vb", "c": "vc"}, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	for i := 0; i < maxTemplateRefs-1; i++ {
		st.UpsertTemplateModrinth(&models.TemplateModrinthRef{TemplateID: tpl.ID, Path: fmt.Sprintf("plugins/p%03d.jar", i), ProjectID: fmt.Sprint("p", i)})
	}
	skipped := extractZip(t, h, tpl, []zipEntry{
		{name: "plugins/p000.jar", body: []byte("a")}, // replaces, not new
		{name: "plugins/new1.jar", body: []byte("b")}, // the 500th
		{name: "plugins/new2.jar", body: []byte("c")}, // one too many
	}, services.TemplatePolicyNoJars)
	reasons := reasonsByPath(skipped)
	if len(reasons) != 1 || reasons["plugins/new2.jar"] != refCapReason {
		t.Fatalf("skipped %v, want only new2 over the cap", skipped)
	}
	refs, _ := st.ListTemplateModrinth(tpl.ID)
	if len(refs) != maxTemplateRefs {
		t.Fatalf("%d refs, want %d", len(refs), maxTemplateRefs)
	}
	for _, r := range refs {
		if r.Path == "plugins/p000.jar" && r.VersionID != "va" {
			t.Fatalf("p000 not replaced: %+v", r)
		}
		if r.Path == "plugins/new2.jar" {
			t.Fatal("new2 stored past the cap")
		}
	}
}
