package storageapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"
)

// TestCoreStorage_SaveConfig_SecretOnlyRotationWritesTheNewSecret pins the
// exact drift the persistCoreStorageConfig extraction exists to prevent. A
// body carrying ONLY s3SecretKey has no backend, so mergeCoreStorageCandidate
// returns the STORED config wholesale and the merged struct's S3SecretKey is
// the OLD secret. Handing that struct plus a "yes, write a secret" flag to the
// writer therefore rewrites the old secret over itself and the rotation
// silently no-ops with a 200. The writer takes the secret VALUE instead, so the
// only secret it can write is the one the request actually submitted.
//
// The panel always sends backend, so this is reachable only from an API-key or
// script client - which is precisely why a test has to hold it.
func TestCoreStorage_SaveConfig_SecretOnlyRotationWritesTheNewSecret(t *testing.T) {
	fs := newCoreStorageTargetFakeStore()
	fs.kv[handlers.KeyCoreStorageBackend] = "s3"
	fs.kv[handlers.KeyCoreStorageS3Endpoint] = "https://s3.example.com"
	fs.kv[handlers.KeyCoreStorageS3Bucket] = "bucket1"
	fs.kv[handlers.KeyCoreStorageS3AccessKey] = "key1"
	fs.kv[handlers.KeyCoreStorageS3SecretKey] = "OLD-SECRET"
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")})

	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage",
		strings.NewReader(`{"s3SecretKey":"ROTATED-SECRET"}`)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[handlers.KeyCoreStorageS3SecretKey] != "ROTATED-SECRET" {
		t.Fatalf("stored secret = %q, want %q: a secret-only rotation returned 200 but left the old secret in place",
			fs.kv[handlers.KeyCoreStorageS3SecretKey], "ROTATED-SECRET")
	}
}
