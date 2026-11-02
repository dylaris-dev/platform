package storageapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/services/corestorage"
)

// TestCoreStorage_SaveConfig_SwitchToPathClearsStoredS3Secret guards Minor 6:
// switching the backend away from s3 must clear the stored S3 secret too, so
// it doesn't linger orphaned (with S3SecretSet still reporting true) once
// the s3 config it belonged to is no longer in use.
func TestCoreStorage_SaveConfig_SwitchToPathClearsStoredS3Secret(t *testing.T) {
	fs := handlertest.NewSettingsStore()
	seedCoreStorageS3(fs)
	// A Redis with one heartbeat: saving a host-path backend now consults the
	// online-Core count, and refuses when it cannot be taken at all.
	h := NewCoreStorageHandler(withCoreStorage(&handlers.AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")}))

	dir := handlertest.ConnectionProbeDir(t)
	body, _ := json.Marshal(corestorage.Config{Backend: "path", Path: dir, PathConfirmed: true})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.KV[corestorage.KeyS3SecretKey] != "" {
		t.Errorf("stored s3 secret not cleared after switching to path backend, got %q", fs.KV[corestorage.KeyS3SecretKey])
	}
}

// TestCoreStorage_SaveConfig_TrimsS3FieldWhitespace guards Fix 2: a
// clipboard-pasted key/secret/endpoint/bucket with surrounding whitespace
// must be trimmed before persisting, not silently break auth later.
func TestCoreStorage_SaveConfig_TrimsS3FieldWhitespace(t *testing.T) {
	fs := handlertest.NewSettingsStore()
	h := NewCoreStorageHandler(withCoreStorage(&handlers.AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")}))

	body, _ := json.Marshal(corestorage.Config{
		Backend: "s3", S3Endpoint: "  https://s3.example.com  ", S3Bucket: "  bucket1 ",
		S3AccessKey: " key1 ", S3SecretKey: " sekret ",
	})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.KV[corestorage.KeyS3Endpoint] != "https://s3.example.com" {
		t.Errorf("s3Endpoint not trimmed, got %q", fs.KV[corestorage.KeyS3Endpoint])
	}
	if fs.KV[corestorage.KeyS3Bucket] != "bucket1" {
		t.Errorf("s3Bucket not trimmed, got %q", fs.KV[corestorage.KeyS3Bucket])
	}
	if fs.KV[corestorage.KeyS3AccessKey] != "key1" {
		t.Errorf("s3AccessKey not trimmed, got %q", fs.KV[corestorage.KeyS3AccessKey])
	}
	if fs.KV[corestorage.KeyS3SecretKey] != "sekret" {
		t.Errorf("s3SecretKey not trimmed, got %q", fs.KV[corestorage.KeyS3SecretKey])
	}
}
