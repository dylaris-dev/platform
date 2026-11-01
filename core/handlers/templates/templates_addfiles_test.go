package templates

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-core/services"
)

type part struct{ field, name, body string }

func addRequest(t *testing.T, parts []part) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, p := range parts {
		if p.field == "file" {
			fw, err := mw.CreateFormFile("file", p.name)
			if err != nil {
				t.Fatal(err)
			}
			fw.Write([]byte(p.body))
		} else if err := mw.WriteField(p.field, p.body); err != nil {
			t.Fatal(err)
		}
	}
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/templates/1/files", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

func TestAddFilesBundle(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	rec := httptest.NewRecorder()
	h.AddFiles(rec, withID(asUser(addRequest(t, []part{
		{"path", "", "config/a.yml"}, {"file", "a.yml", "a: 1"},
		{"path", "", "../evil"}, {"file", "evil", "x"},
		{"path", "", "plugins/Thing.jar"}, {"file", "Thing.jar", "jar"},
		{"path", "", "deep/er/b.txt"}, {"file", "b.txt", "bb"},
	}), ownerID, false), tpl.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body)
	}
	if got := strings.Join(storedFiles(t, h, tpl), ","); got != "config/a.yml,deep/er/b.txt" {
		t.Errorf("stored %q", got)
	}
	for _, want := range []string{"../evil", "plugins/Thing.jar"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("response does not list %q: %s", want, rec.Body)
		}
	}
}

func TestAddFilesLegacyDir(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	rec := httptest.NewRecorder()
	h.AddFiles(rec, withID(asUser(addRequest(t, []part{
		{"dir", "", "config"}, {"file", "a.yml", "a: 1"},
	}), ownerID, false), tpl.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body)
	}
	if got := strings.Join(storedFiles(t, h, tpl), ","); got != "config/a.yml" {
		t.Errorf("stored %q", got)
	}
}

func TestAddFilesMismatchedPaths(t *testing.T) {
	h, st := newTestHandler(t, nil)
	tpl := seedTemplate(t, st, ownerID, "personal")
	rec := httptest.NewRecorder()
	h.AddFiles(rec, withID(asUser(addRequest(t, []part{
		{"path", "", "a.yml"}, {"file", "a.yml", "1"}, {"file", "b.yml", "2"},
	}), ownerID, false), tpl.ID))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d (%s), want 400", rec.Code, rec.Body)
	}
	if n := len(storedFiles(t, h, tpl)); n != 0 {
		t.Errorf("%d files stored", n)
	}
}

// stagedLeft counts staging files left in dir.
func stagedLeft(t *testing.T, dir string) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "template-add-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m)
}

// withSmallBundleCap points staging at a private dir and shrinks the bundle cap.
func withSmallBundleCap(t *testing.T, cap int64) string {
	t.Helper()
	dir := t.TempDir()
	oldDir, oldCap := stageDir, bundleBytesCap
	stageDir, bundleBytesCap = dir, cap
	t.Cleanup(func() { stageDir, bundleBytesCap = oldDir, oldCap })
	return dir
}

func TestAddFilesRefusalsLeaveNoStagedFiles(t *testing.T) {
	var many []part
	for i := 0; i <= maxBundleFiles; i++ {
		many = append(many, part{"path", "", fmt.Sprintf("f%d.txt", i)}, part{"file", "x", "x"})
	}
	var noise []part
	for i := 0; i < 2*maxBundleFiles+5; i++ {
		noise = append(noise, part{"junk", "", "x"})
	}
	big := strings.Repeat("a", 60<<10)
	tests := []struct {
		name  string
		parts []part
		want  int
	}{
		{"201 files", many, http.StatusRequestEntityTooLarge},
		{"unknown parts are counted", noise, http.StatusRequestEntityTooLarge},
		{"two files over the bundle cap", []part{{"path", "", "a"}, {"file", "a", big}, {"path", "", "b"}, {"file", "b", big}}, http.StatusRequestEntityTooLarge},
		{"mismatched counts", []part{{"path", "", "a.txt"}, {"file", "a", "1"}, {"file", "b", "2"}}, http.StatusBadRequest},
		{"duplicate target", []part{{"path", "", "a.txt"}, {"file", "a", "1"}, {"path", "", "./a.txt"}, {"file", "b", "2"}}, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := withSmallBundleCap(t, 100<<10)
			h, st := newTestHandler(t, nil)
			tpl := seedTemplate(t, st, ownerID, "personal")
			rec := httptest.NewRecorder()
			h.AddFiles(rec, withID(asUser(addRequest(t, tc.parts), ownerID, false), tpl.ID))
			if rec.Code != tc.want {
				t.Fatalf("got %d (%s), want %d", rec.Code, rec.Body, tc.want)
			}
			if n := len(storedFiles(t, h, tpl)); n != 0 {
				t.Errorf("%d files stored", n)
			}
			if n := stagedLeft(t, dir); n != 0 {
				t.Errorf("%d staged files left", n)
			}
		})
	}
}

func TestAddFilesSingleFileMayExceedTheBundleCap(t *testing.T) {
	dir := withSmallBundleCap(t, 100<<10)
	h, st := newTestHandler(t, nil) // default template cap is far above 300 KiB
	tpl := seedTemplate(t, st, ownerID, "personal")
	rec := httptest.NewRecorder()
	h.AddFiles(rec, withID(asUser(addRequest(t, []part{
		{"path", "", "world/big.dat"}, {"file", "big.dat", strings.Repeat("a", 300<<10)},
	}), ownerID, false), tpl.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s), want 200", rec.Code, rec.Body)
	}
	if got := strings.Join(storedFiles(t, h, tpl), ","); got != "world/big.dat" {
		t.Errorf("stored %q", got)
	}
	if n := stagedLeft(t, dir); n != 0 {
		t.Errorf("%d staged files left", n)
	}
}

func TestAddFilesRefusesOversizedDeclaredBody(t *testing.T) {
	h, st := newTestHandler(t, map[string]string{services.TemplatesMaxSizeMBKey: "1"})
	tpl := seedTemplate(t, st, ownerID, "personal")
	r := httptest.NewRequest(http.MethodPost, "/x", untouchedBody{t})
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	r.ContentLength = 1<<20 + uploadSlack + 1
	rec := httptest.NewRecorder()
	h.AddFiles(rec, withID(asUser(r, ownerID, false), tpl.ID))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("got %d, want 413", rec.Code)
	}
}

func TestAddFilesMidCopyOverflowLeavesNoStagedFiles(t *testing.T) {
	dir := withSmallBundleCap(t, 1<<30) // the stream limit, not the bundle cap, must trip
	h, st := newTestHandler(t, map[string]string{services.TemplatesMaxSizeMBKey: "1"})
	tpl := seedTemplate(t, st, ownerID, "personal")
	// Chunked (unknown length) body past the stream limit: MaxBytesError mid-copy.
	req := addRequest(t, []part{
		{"path", "", "a"}, {"file", "a", "small"},
		{"path", "", "b"}, {"file", "b", strings.Repeat("a", 1<<20+uploadSlack+10)},
	})
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.AddFiles(rec, withID(asUser(req, ownerID, false), tpl.ID))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d (%s), want 413", rec.Code, rec.Body)
	}
	if n := stagedLeft(t, dir); n != 0 {
		t.Errorf("%d staged files left", n)
	}
}
