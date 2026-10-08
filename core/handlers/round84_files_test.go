package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
)

// noServerStore knows no server: every caller is refused.
type noServerStore struct{ quotaHTTPFakeStore }

func (noServerStore) GetServerByUUID(string) (*models.Server, error) {
	return nil, errors.New("no such server")
}

type countingBody struct {
	r io.Reader
	n int
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}
func (c *countingBody) Close() error { return nil }

// The upload parsed - spooled to Core's disk - up to the upload limit before it
// asked who was calling, so any signed-in account, one with no server at all,
// could fill Core's temp disk and only then be told 403.
func TestAnUploadIsRefusedBeforeItsBodyIsRead(t *testing.T) {
	rdb, _ := newQuotaHTTPRedis(t)
	st := noServerStore{quotaHTTPFakeStore{newCoreStorageHTTPFakeStore()}}
	h := &FileHandler{state: &AppState{Redis: rdb, Store: st, Authz: authz.NewResolver(st)}}

	req := newUploadRequest(t, "s1", 1<<20)
	body := &countingBody{r: req.Body}
	req.Body = body
	rw := httptest.NewRecorder()
	h.UploadFileHandler(rw, req)
	if rw.Code != http.StatusForbidden || body.n != 0 {
		t.Fatalf("status %d after reading %d bytes, want 403 before any", rw.Code, body.n)
	}

	// server_uuid only in the form would need the body read to be found.
	req = newUploadRequest(t, "s1", 1<<20)
	req.URL.RawQuery = ""
	body = &countingBody{r: req.Body}
	req.Body = body
	rw = httptest.NewRecorder()
	h.UploadFileHandler(rw, req)
	if rw.Code != http.StatusBadRequest || body.n != 0 {
		t.Fatalf("no query: status %d after reading %d bytes", rw.Code, body.n)
	}
}

// Only the upload applied the owner cut-off; save, create, rename and copy
// kept writing to a suspended tenant's server.
func TestEveryFileWriteRefusesASuspendedTenant(t *testing.T) {
	rdb, _ := newQuotaHTTPRedis(t)
	st := suspendedUploadStore{quotaHTTPFakeStore{newCoreStorageHTTPFakeStore()}}
	h := &FileHandler{state: &AppState{Redis: rdb, Store: st, Authz: authz.NewResolver(st)}}

	for name, tc := range map[string]struct {
		call func(http.ResponseWriter, *http.Request)
		body string
	}{
		"save":   {h.SaveFileHandler, `{"path":"a.txt","content":"x"}`},
		"create": {h.CreateFileHandler, `{"path":"a.txt"}`},
		"rename": {h.RenameFileHandler, `{"oldPath":"a.txt","newPath":"b.txt"}`},
		"copy":   {h.CopyFileHandler, `{"oldPath":"a.txt","newPath":"b.txt"}`},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/files/x?server_uuid=s1", strings.NewReader(tc.body))
			ctx := context.WithValue(req.Context(), "username", "u")
			ctx = context.WithValue(ctx, "isAdmin", true)
			// An operator's key carries no override (round 26), unlike a session.
			ctx = context.WithValue(ctx, apiKeyCtxKey{}, &apiKeyCtx{key: &models.APIKey{ID: 1}})
			rw := httptest.NewRecorder()
			tc.call(rw, req.WithContext(ctx))
			if rw.Code != http.StatusForbidden || !strings.Contains(rw.Body.String(), "suspended") {
				t.Fatalf("status %d (%s), want the suspension refusal", rw.Code, rw.Body)
			}
		})
	}
}

// The small file operations decoded their JSON bodies unbounded.
func TestFileOperationBodiesAreBounded(t *testing.T) {
	h := &FileHandler{state: &AppState{}}
	long := strings.Repeat("a", fileOpBodyLimit+1)
	huge := `{"path":"` + long + `","oldPath":"` + long + `","newPath":"b"}`
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"create": h.CreateFileHandler,
		"rename": h.RenameFileHandler,
		"copy":   h.CopyFileHandler,
		"delete": h.DeleteFileHandler,
	} {
		rw := httptest.NewRecorder()
		call(rw, httptest.NewRequest(http.MethodPost, "/x?server_uuid=s1", bytes.NewReader([]byte(huge))))
		if rw.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d for a body over the limit", name, rw.Code)
		}
	}
}
