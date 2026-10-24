package storageapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"
)

// TestCoreStorage_SaveConfig_SwitchToPathClearsStoredS3Secret guards Minor 6:
// switching the backend away from s3 must clear the stored S3 secret too, so
// it doesn't linger orphaned (with S3SecretSet still reporting true) once
// the s3 config it belonged to is no longer in use.
func TestCoreStorage_SaveConfig_SwitchToPathClearsStoredS3Secret(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	seedCoreStorageS3(fs)
	// A Redis with one heartbeat: saving a host-path backend now consults the
	// online-Core count, and refuses when it cannot be taken at all.
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")})

	dir := testConnectionProbeDir(t)
	body, _ := json.Marshal(handlers.CoreStorageConfig{Backend: "path", Path: dir, PathConfirmed: true})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[handlers.KeyCoreStorageS3SecretKey] != "" {
		t.Errorf("stored s3 secret not cleared after switching to path backend, got %q", fs.kv[handlers.KeyCoreStorageS3SecretKey])
	}
}

// TestCoreStorage_SaveConfig_TrimsS3FieldWhitespace guards Fix 2: a
// clipboard-pasted key/secret/endpoint/bucket with surrounding whitespace
// must be trimmed before persisting, not silently break auth later.
func TestCoreStorage_SaveConfig_TrimsS3FieldWhitespace(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")})

	body, _ := json.Marshal(handlers.CoreStorageConfig{
		Backend: "s3", S3Endpoint: "  https://s3.example.com  ", S3Bucket: "  bucket1 ",
		S3AccessKey: " key1 ", S3SecretKey: " sekret ",
	})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[handlers.KeyCoreStorageS3Endpoint] != "https://s3.example.com" {
		t.Errorf("s3Endpoint not trimmed, got %q", fs.kv[handlers.KeyCoreStorageS3Endpoint])
	}
	if fs.kv[handlers.KeyCoreStorageS3Bucket] != "bucket1" {
		t.Errorf("s3Bucket not trimmed, got %q", fs.kv[handlers.KeyCoreStorageS3Bucket])
	}
	if fs.kv[handlers.KeyCoreStorageS3AccessKey] != "key1" {
		t.Errorf("s3AccessKey not trimmed, got %q", fs.kv[handlers.KeyCoreStorageS3AccessKey])
	}
	if fs.kv[handlers.KeyCoreStorageS3SecretKey] != "sekret" {
		t.Errorf("s3SecretKey not trimmed, got %q", fs.kv[handlers.KeyCoreStorageS3SecretKey])
	}
}
