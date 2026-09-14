package handlers

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-pkg/beam/quota"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// These tests pin that the browser HTTP upload path enforces the same beam
// upload limits the node enforces on the tunnel path, so the quota cannot be
// evaded by uploading through a browser. Both rejections must fire BEFORE the
// handler contacts the node, so a nil GRPCRegistry never runs. An admin still
// goes through the resolver, whose admin short-circuit needs nothing from the
// store beyond the server row, so a fake answering GetSetting and that is enough.

func newQuotaHTTPRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

func newUploadRequest(t *testing.T, serverUUID string, fileSize int) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("files", "world.zip")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte("a"), fileSize)); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_ = mw.WriteField("server_uuid", serverUUID)
	_ = mw.WriteField("path", "")
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/files/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ctx := context.WithValue(req.Context(), "username", "u")
	ctx = context.WithValue(ctx, "isAdmin", true) // the resolver's admin short-circuit
	return req.WithContext(ctx)
}

func TestUploadFileHandler_DailyQuotaRejects(t *testing.T) {
	rdb, mr := newQuotaHTTPRedis(t)
	mr.Set(quota.DailyUploadBytesKey, "1000")
	mr.Set(quota.DailyKey("u", time.Now()), "900")

	h := &FileHandler{state: quotaHTTPState(rdb)}
	rw := httptest.NewRecorder()
	h.UploadFileHandler(rw, newUploadRequest(t, "s1", 200)) // 900 + 200 > 1000

	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", rw.Code, rw.Body.String())
	}
	// Rejected uploads must not be counted (the increment runs only on success).
	if got, _ := rdb.Get(context.Background(), quota.DailyKey("u", time.Now())).Int64(); got != 900 {
		t.Errorf("counter changed to %d, want 900 (no increment on reject)", got)
	}
}

func TestUploadFileHandler_SizeCapRejects(t *testing.T) {
	rdb, mr := newQuotaHTTPRedis(t)
	mr.Set(quota.MaxUploadBytesKey, "100")

	h := &FileHandler{state: quotaHTTPState(rdb)}
	rw := httptest.NewRecorder()
	h.UploadFileHandler(rw, newUploadRequest(t, "s1", 200)) // single 200-byte file > 100 cap

	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", rw.Code, rw.Body.String())
	}
}

type quotaHTTPFakeStore struct {
	*coreStorageHTTPFakeStore
}

func (f quotaHTTPFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	return &models.Server{ID: 1, UUID: uuid, OwnerID: "someone-else"}, nil
}

func quotaHTTPState(rdb *redis.Client) *AppState {
	st := quotaHTTPFakeStore{newCoreStorageHTTPFakeStore()}
	return &AppState{Redis: rdb, Store: st, Authz: authz.NewResolver(st)}
}
