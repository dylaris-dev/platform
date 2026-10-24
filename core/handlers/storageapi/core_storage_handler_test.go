package storageapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-core/handlers"
)

func TestCoreStorage_SaveThenGet_BlanksSecretAndKeepsExisting(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	// A single online Core so checkSharedStorageReachable's short-circuit
	// applies; this test is about the secret round trip, not the
	// reachability round.
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")})

	// Save an s3 config with a secret.
	body, _ := json.Marshal(handlers.CoreStorageConfig{
		Backend: "s3", S3Bucket: "b", S3AccessKey: "k", S3SecretKey: "sekret",
	})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("SaveConfig status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[handlers.KeyCoreStorageS3SecretKey] != "sekret" {
		t.Fatalf("secret not persisted, got %q", fs.kv[handlers.KeyCoreStorageS3SecretKey])
	}

	// GET must not leak the secret but must flag it as set.
	rw = httptest.NewRecorder()
	h.GetConfig(rw, httptest.NewRequest(http.MethodGet, "/api/settings/core-storage", nil))
	var got struct {
		Settings handlers.CoreStorageConfig `json:"settings"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &got)
	if got.Settings.S3SecretKey != "" {
		t.Errorf("GET leaked secret: %q", got.Settings.S3SecretKey)
	}
	if !got.Settings.S3SecretSet {
		t.Errorf("GET S3SecretSet = false, want true")
	}
	// Raw-body check, not just the decoded struct: catches a future field
	// added under some other key that would still surface the secret.
	if strings.Contains(rw.Body.String(), "sekret") {
		t.Errorf("GET response body leaked the secret somewhere: %s", rw.Body.String())
	}

	// Re-save with a blank secret and the SAME endpoint/bucket/accessKey (Fix
	// 1 only backfills the secret when the identity fields are unchanged):
	// the stored secret must survive, and an unrelated field (region) still
	// updates.
	body, _ = json.Marshal(handlers.CoreStorageConfig{Backend: "s3", S3Bucket: "b", S3AccessKey: "k", S3Region: "eu-central-1", S3SecretKey: ""})
	rw = httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("re-save status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[handlers.KeyCoreStorageS3SecretKey] != "sekret" {
		t.Errorf("blank re-save wiped the secret, got %q", fs.kv[handlers.KeyCoreStorageS3SecretKey])
	}
	if fs.kv[handlers.KeyCoreStorageS3Region] != "eu-central-1" {
		t.Errorf("region not updated on re-save, got %q", fs.kv[handlers.KeyCoreStorageS3Region])
	}
}

func TestCoreStorage_SaveRejectsInvalid(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})
	body, _ := json.Marshal(handlers.CoreStorageConfig{Backend: "path", Path: "relative", PathConfirmed: true})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("SaveConfig invalid status = %d, want 400", rw.Code)
	}
	if fs.kv[handlers.KeyCoreStorageBackend] != "" {
		t.Errorf("invalid config was partially persisted: %v", fs.kv)
	}
}

// TestCoreStorage_SaveRejectsInvalid_DoesNotWipeExistingValidConfig guards the
// "reject before persist" contract from the other direction: an admin who
// already has a valid config saved and submits a bad edit must not lose the
// previously-good settings.
func TestCoreStorage_SaveRejectsInvalid_DoesNotWipeExistingValidConfig(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	fs.kv[handlers.KeyCoreStorageBackend] = "path"
	fs.kv[handlers.KeyCoreStoragePath] = "/mnt/shared"
	fs.kv[handlers.KeyCoreStoragePathConfirm] = "true"
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})

	body, _ := json.Marshal(handlers.CoreStorageConfig{Backend: "path", Path: "relative", PathConfirmed: true})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rw.Code)
	}
	if fs.kv[handlers.KeyCoreStoragePath] != "/mnt/shared" {
		t.Errorf("invalid save clobbered the existing good path, got %q", fs.kv[handlers.KeyCoreStoragePath])
	}
}

func TestCoreStorage_GetConfig_NeverConfigured(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})
	rw := httptest.NewRecorder()
	h.GetConfig(rw, httptest.NewRequest(http.MethodGet, "/api/settings/core-storage", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	var got struct {
		Settings handlers.CoreStorageConfig `json:"settings"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &got)
	if got.Settings.S3SecretSet {
		t.Errorf("S3SecretSet = true on a never-configured store, want false")
	}
}

func TestCoreStorage_TestConnection_PathProbe(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	dir := testConnectionProbeDir(t)
	fs.kv[handlers.KeyCoreStorageBackend] = "path"
	fs.kv[handlers.KeyCoreStoragePath] = dir
	fs.kv[handlers.KeyCoreStoragePathConfirm] = "true"
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})

	rw := httptest.NewRecorder()
	h.TestConnection(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage/test", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("TestConnection status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "\"ok\":true") {
		t.Errorf("TestConnection body = %s, want ok:true", rw.Body.String())
	}
}

// TestCoreStorage_TestConnection_UsesCandidateFromBody_NotStoredConfig is the
// key regression guard for "test before saving": the store has no valid
// config at all, so a success here can only come from the request body.
func TestCoreStorage_TestConnection_UsesCandidateFromBody_NotStoredConfig(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	dir := testConnectionProbeDir(t)
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})

	body, _ := json.Marshal(handlers.CoreStorageConfig{Backend: "path", Path: dir, PathConfirmed: true})
	rw := httptest.NewRecorder()
	h.TestConnection(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage/test", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "\"ok\":true") {
		t.Errorf("body = %s, want ok:true", rw.Body.String())
	}
}

// TestCoreStorage_TestConnection_InvalidCandidateReportsFailure_NotHTTPError
// asserts an invalid candidate reports ok:false in a 200 JSON body (a "test"
// result), not a hard HTTP error - the panel renders this inline.
func TestCoreStorage_TestConnection_InvalidCandidateReportsFailure_NotHTTPError(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})
	body, _ := json.Marshal(handlers.CoreStorageConfig{Backend: "path", Path: "relative", PathConfirmed: true})
	rw := httptest.NewRecorder()
	h.TestConnection(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage/test", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (invalid candidate reports ok:false in-band)", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "\"ok\":false") {
		t.Errorf("body = %s, want ok:false", rw.Body.String())
	}
}

func TestCoreStorage_SaveConfig_BlankSecretUnchangedIdentity_ReusesStoredSecret(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	seedCoreStorageS3(fs)
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")})

	body, _ := json.Marshal(handlers.CoreStorageConfig{
		Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "bucket1", S3AccessKey: "key1",
		S3Region: "eu-west-1", // an unrelated field changes, proving the save actually went through
	})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[handlers.KeyCoreStorageS3SecretKey] != "stored-secret" {
		t.Errorf("stored secret not reused, got %q", fs.kv[handlers.KeyCoreStorageS3SecretKey])
	}
	if fs.kv[handlers.KeyCoreStorageS3Region] != "eu-west-1" {
		t.Errorf("unrelated field did not persist, got %q", fs.kv[handlers.KeyCoreStorageS3Region])
	}
}

func TestCoreStorage_SaveConfig_BlankSecretChangedAccessKey_Rejected(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	seedCoreStorageS3(fs)
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})

	body, _ := json.Marshal(handlers.CoreStorageConfig{
		Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "bucket1", S3AccessKey: "key2",
	})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (changed access key + blank secret must be rejected); body=%s", rw.Code, rw.Body.String())
	}
	if fs.kv[handlers.KeyCoreStorageS3AccessKey] != "key1" {
		t.Errorf("rejected save persisted the new access key anyway, got %q", fs.kv[handlers.KeyCoreStorageS3AccessKey])
	}
	if fs.kv[handlers.KeyCoreStorageS3SecretKey] != "stored-secret" {
		t.Errorf("rejected save touched the stored secret, got %q", fs.kv[handlers.KeyCoreStorageS3SecretKey])
	}
}

func TestCoreStorage_SaveConfig_BlankSecretChangedEndpoint_Rejected(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	seedCoreStorageS3(fs)
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})

	body, _ := json.Marshal(handlers.CoreStorageConfig{
		Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "bucket1", S3AccessKey: "key1",
	})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (changed endpoint + blank secret must be rejected); body=%s", rw.Code, rw.Body.String())
	}
	if fs.kv[handlers.KeyCoreStorageS3Endpoint] != "https://s3.example.com" {
		t.Errorf("rejected save persisted the new endpoint anyway, got %q", fs.kv[handlers.KeyCoreStorageS3Endpoint])
	}
}

func TestCoreStorage_TestConnection_BlankSecretChangedAccessKey_Rejected(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	seedCoreStorageS3(fs)
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})

	body, _ := json.Marshal(handlers.CoreStorageConfig{
		Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "bucket1", S3AccessKey: "key2",
	})
	rw := httptest.NewRecorder()
	h.TestConnection(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage/test", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (in-band failure); body=%s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "\"ok\":false") {
		t.Errorf("body = %s, want ok:false (changed access key + blank secret must be rejected, not probed with the real secret)", rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "access key + secret are required") {
		t.Errorf("body = %s, want the validation error (proves rejection happened before any probe, not a network failure)", rw.Body.String())
	}
}

func TestCoreStorage_TestConnection_BlankSecretChangedEndpoint_Rejected(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	seedCoreStorageS3(fs)
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})

	body, _ := json.Marshal(handlers.CoreStorageConfig{
		Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "bucket1", S3AccessKey: "key1",
	})
	rw := httptest.NewRecorder()
	h.TestConnection(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage/test", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (in-band failure); body=%s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "access key + secret are required") {
		t.Errorf("body = %s, want the validation error (changed endpoint + blank secret must be rejected before probing, i.e. never dial the attacker endpoint using the real secret)", rw.Body.String())
	}
}

// TestCoreStorage_TestConnection_BlankSecretUnchangedIdentity_PassesValidation
// proves the reused secret actually clears validateCoreStorageConfig (unlike
// the "changed" cases above): the probe proceeds to an actual write attempt
// against a loopback port nothing listens on, so it fails fast on a
// connection error rather than the "access key + secret are required"
// validation error - that is the observable difference between "merge
// backfilled the secret" and "merge left it blank".
func TestCoreStorage_TestConnection_BlankSecretUnchangedIdentity_PassesValidation(t *testing.T) {
	disableAWSRetries(t)
	fs := newCoreStorageHTTPFakeStore()
	fs.kv[handlers.KeyCoreStorageBackend] = "s3"
	fs.kv[handlers.KeyCoreStorageS3Endpoint] = "http://127.0.0.1:1"
	fs.kv[handlers.KeyCoreStorageS3Bucket] = "bucket1"
	fs.kv[handlers.KeyCoreStorageS3AccessKey] = "key1"
	fs.kv[handlers.KeyCoreStorageS3SecretKey] = "stored-secret"
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})

	body, _ := json.Marshal(handlers.CoreStorageConfig{
		Backend: "s3", S3Endpoint: "http://127.0.0.1:1", S3Bucket: "bucket1", S3AccessKey: "key1",
	})
	rw := httptest.NewRecorder()
	h.TestConnection(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage/test", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	if strings.Contains(rw.Body.String(), "access key + secret are required") {
		t.Errorf("body = %s, unchanged identity + blank secret should reuse the stored secret and pass validation, not be rejected", rw.Body.String())
	}
}

// TestCoreStorage_TestConnection_FailureBody_NeverLeaksSecret guards the raw
// wire body, not just the CoreStorageConfig struct: a future change that
// surfaces the secret under some other key must still be caught.
func TestCoreStorage_TestConnection_FailureBody_NeverLeaksSecret(t *testing.T) {
	disableAWSRetries(t)
	fs := newCoreStorageHTTPFakeStore()
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})

	const theSecret = "unleaked-topsecret-value"
	body, _ := json.Marshal(handlers.CoreStorageConfig{
		Backend: "s3", S3Endpoint: "http://127.0.0.1:1", S3Bucket: "bucket1", S3AccessKey: "key1",
		S3SecretKey: theSecret,
	})
	rw := httptest.NewRecorder()
	h.TestConnection(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage/test", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "\"ok\":false") {
		t.Fatalf("body = %s, want ok:false (loopback port 1 refuses the connection)", rw.Body.String())
	}
	if strings.Contains(rw.Body.String(), theSecret) {
		t.Errorf("TestConnection failure body leaked the secret: %s", rw.Body.String())
	}
}

// TestCoreStorage_TestConnection_PathProbe_CleansUpProbeDir guards Fix 4: a
// tested (whether saved or not) path backend must not leave an empty
// "_probe" directory behind forever.
func TestCoreStorage_TestConnection_PathProbe_CleansUpProbeDir(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	dir := testConnectionProbeDir(t)
	fs.kv[handlers.KeyCoreStorageBackend] = "path"
	fs.kv[handlers.KeyCoreStoragePath] = dir
	fs.kv[handlers.KeyCoreStoragePathConfirm] = "true"
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})

	rw := httptest.NewRecorder()
	h.TestConnection(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage/test", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("TestConnection status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "_probe")); !os.IsNotExist(err) {
		t.Errorf("_probe directory left behind after test-connection, stat err = %v", err)
	}
}

// TestCoreStorage_TestConnection_WarnsOnAnEphemeralPath covers the trap the
// probe alone cannot see.
//
// probeStorageProvider writes, reads back and deletes a file. A directory that
// exists only in the container's own writable layer passes all three, so an
// operator who configured /mnt/nas-share but never added the bind mount got a
// plain green result on storage that disappears at the next container
// recreation. The warning has to ride along WITH ok:true, because the probe
// genuinely did succeed - downgrading it to a failure would be wrong, and an
// ephemeral path is a legitimate choice for a throwaway dev instance.
func TestCoreStorage_TestConnection_WarnsOnAnEphemeralPath(t *testing.T) {
	cases := []struct {
		name string
		// onRoot/determinable are what the per-OS probe reports.
		onRoot       bool
		determinable bool
		backend      string
		wantWarning  bool
	}{
		{name: "ephemeral path warns", onRoot: true, determinable: true, backend: "path", wantWarning: true},
		{name: "mounted path does not warn", onRoot: false, determinable: true, backend: "path"},
		// Off Linux the question is unanswerable. A guess either way is worse
		// than silence: a false warning trains operators to ignore it.
		{name: "undeterminable does not warn", onRoot: true, determinable: false, backend: "path"},
		// s3 has no local path at all, so the check must not even be consulted.
		{name: "s3 never warns", onRoot: true, determinable: true, backend: "s3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := pathOnContainerRootFS
			t.Cleanup(func() { pathOnContainerRootFS = orig })
			called := false
			pathOnContainerRootFS = func(string) (bool, bool) {
				called = true
				return c.onRoot, c.determinable
			}

			fs := newCoreStorageHTTPFakeStore()
			h := NewCoreStorageHandler(&handlers.AppState{Store: fs})
			var body []byte
			if c.backend == "s3" {
				// A loopback port nothing listens on, the same shape the other
				// s3 tests here use. The probe fails, which is fine: the point
				// is that an s3 config has no local path, so the check must not
				// be consulted at all - asserted via `called` below.
				disableAWSRetries(t)
				body, _ = json.Marshal(handlers.CoreStorageConfig{
					Backend: "s3", S3Endpoint: "http://127.0.0.1:1", S3Bucket: "b",
					S3AccessKey: "k", S3SecretKey: "s",
				})
			} else {
				body, _ = json.Marshal(handlers.CoreStorageConfig{
					Backend: "path", Path: testConnectionProbeDir(t), PathConfirmed: true,
				})
			}

			rw := httptest.NewRecorder()
			h.TestConnection(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage/test", bytes.NewReader(body)))
			if rw.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
			}
			got := rw.Body.String()

			if c.backend == "s3" && called {
				t.Errorf("the ephemeral-path check ran for an s3 config, which has no local path")
			}
			hasWarning := strings.Contains(got, "\"warning\"")
			if hasWarning != c.wantWarning {
				t.Fatalf("warning present = %v, want %v; body = %s", hasWarning, c.wantWarning, got)
			}
			if !c.wantWarning {
				return
			}
			// The warning must not contradict the probe result it rides with.
			if !strings.Contains(got, "\"ok\":true") {
				t.Errorf("the warning replaced the probe verdict; body = %s", got)
			}
			for _, want := range []string{"LOST", "bind mount"} {
				if !strings.Contains(got, want) {
					t.Errorf("warning does not mention %q, so it is not actionable; body = %s", want, got)
				}
			}
		})
	}
}
