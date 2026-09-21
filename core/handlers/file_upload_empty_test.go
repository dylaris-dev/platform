package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// An upload that carries nothing is a failed upload.
//
// It used to fall through the loop and answer 200 with
// "0 files uploaded successfully" - measured on production by sending the part
// as "file" rather than "files", which is the obvious mistake to make against
// this endpoint. The only signal a client had was the 0 in a sentence whose
// other word was "successfully".
//
// The state here has no GRPCRegistry on purpose: a refusal that arrives without
// one proves the check happens before anything is sent to a node.
func TestUploadWithNoRecognisedFilePartIsRefused(t *testing.T) {
	cases := []struct {
		name      string
		partName  string
		wantParts bool
	}{
		{"the part is named file, not files", "file", false},
		{"no file part at all", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			if c.partName != "" {
				fw, err := mw.CreateFormFile(c.partName, "world.zip")
				if err != nil {
					t.Fatalf("CreateFormFile: %v", err)
				}
				_, _ = fw.Write([]byte("content"))
			}
			_ = mw.WriteField("server_uuid", "s1")
			_ = mw.WriteField("path", "")
			if err := mw.Close(); err != nil {
				t.Fatalf("close writer: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/api/files/upload", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			ctx := context.WithValue(req.Context(), "username", "u")
			ctx = context.WithValue(ctx, "isAdmin", true)
			req = req.WithContext(ctx)

			rdb, _ := newQuotaHTTPRedis(t)
			h := &FileHandler{state: quotaHTTPState(rdb)}
			rec := httptest.NewRecorder()

			h.UploadFileHandler(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var out struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if out.Success {
				t.Error("an upload that moved nothing reported success")
			}
			if out.Message == "" {
				t.Error("the refusal has to name the field, or the caller repeats the same mistake")
			}
		})
	}
}
