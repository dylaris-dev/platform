package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSaveFileHandler_RejectsOversizedBody pins that /api/files/save bounds its
// request body. The whole file content arrives inline as JSON, so without the
// MaxBytesReader an authenticated user could POST an arbitrarily large body and
// have it decoded into RAM. The reject fires at decode, before the handler
// touches the node, so a nil GRPCRegistry never runs.
func TestSaveFileHandler_RejectsOversizedBody(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	fs.kv["fm.admin_upload_limit"] = "100" // tiny ceiling so the test body is small

	h := &FileHandler{state: &AppState{Store: fs}}

	body := `{"path":"server.properties","content":"` + strings.Repeat("x", 300) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files/save", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), "isAdmin", true))

	rw := httptest.NewRecorder()
	h.SaveFileHandler(rw, req)

	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", rw.Code, rw.Body.String())
	}
}

// The editor's save is capped at saveBodyLimit whatever the upload limit says:
// it decodes the whole body into one string and copies it again, so at the
// default upload limit (500 MB) one request held over a gigabyte of Core's
// memory, and nothing bounded how many ran at once.
func TestSaveFileHandler_IsCappedBelowTheUploadLimit(t *testing.T) {
	for _, upload := range []string{"unlimited", "1073741824"} {
		fs := newCoreStorageHTTPFakeStore()
		fs.kv["fm.user_upload_limit"] = upload
		h := &FileHandler{state: &AppState{Store: fs}}

		req := httptest.NewRequest(http.MethodPost, "/api/files/save", strings.NewReader("{}"))
		req.ContentLength = saveBodyLimit + 1
		req = req.WithContext(context.WithValue(req.Context(), "isAdmin", false))
		rw := httptest.NewRecorder()
		h.SaveFileHandler(rw, req)
		if rw.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("upload limit %s: status %d, want 413", upload, rw.Code)
		}
	}
}
