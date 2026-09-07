package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-core/storage"
	"dylaris-core/store"
)

// coreStorageHTTPFakeStore is a read/write settings fake for the CRUD +
// test-connection handler tests below. Distinct from coreStorageFakeStore
// (core_storage_builder_test.go, GetSetting-only, read-side tests) and from
// modeFakeStore (permissions_mode_test.go, always returns one fixed value):
// this one records SetSetting writes into a map so a save-then-get round
// trip can be asserted.
type coreStorageHTTPFakeStore struct {
	store.Store
	kv map[string]string
}

func newCoreStorageHTTPFakeStore() *coreStorageHTTPFakeStore {
	return &coreStorageHTTPFakeStore{kv: map[string]string{}}
}

func (f *coreStorageHTTPFakeStore) GetSetting(key string) (string, error) { return f.kv[key], nil }

// SetSettingBy is the audited write path; the fake records the value and drops
// the actor, so a handler that writes through it fails its assertion rather than
// panicking on a nil embedded Store.
func (f *coreStorageHTTPFakeStore) SetSettingBy(key, value, _ string) error {
	return f.SetSetting(key, value)
}

func (f *coreStorageHTTPFakeStore) SetSetting(key, value string) error {
	f.kv[key] = value
	return nil
}

func TestCoreStorage_SaveThenGet_BlanksSecretAndKeepsExisting(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	// A single online Core so checkSharedStorageReachable's short-circuit
	// applies; this test is about the secret round trip, not the
	// reachability round.
	h := NewCoreStorageHandler(&AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")})

	// Save an s3 config with a secret.
	body, _ := json.Marshal(CoreStorageConfig{
		Backend: "s3", S3Bucket: "b", S3AccessKey: "k", S3SecretKey: "sekret",
	})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("SaveConfig status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[keyCoreStorageS3SecretKey] != "sekret" {
		t.Fatalf("secret not persisted, got %q", fs.kv[keyCoreStorageS3SecretKey])
	}

	// GET must not leak the secret but must flag it as set.
	rw = httptest.NewRecorder()
	h.GetConfig(rw, httptest.NewRequest(http.MethodGet, "/api/settings/core-storage", nil))
	var got struct {
		Settings CoreStorageConfig `json:"settings"`
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
	body, _ = json.Marshal(CoreStorageConfig{Backend: "s3", S3Bucket: "b", S3AccessKey: "k", S3Region: "eu-central-1", S3SecretKey: ""})
	rw = httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("re-save status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[keyCoreStorageS3SecretKey] != "sekret" {
		t.Errorf("blank re-save wiped the secret, got %q", fs.kv[keyCoreStorageS3SecretKey])
	}
	if fs.kv[keyCoreStorageS3Region] != "eu-central-1" {
		t.Errorf("region not updated on re-save, got %q", fs.kv[keyCoreStorageS3Region])
	}
}

// TestCoreStorage_SaveConfig_SwitchToPathClearsStoredS3Secret guards Minor 6:
// switching the backend away from s3 must clear the stored S3 secret too, so
// it doesn't linger orphaned (with S3SecretSet still reporting true) once
// the s3 config it belonged to is no longer in use.
func TestCoreStorage_SaveConfig_SwitchToPathClearsStoredS3Secret(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	seedCoreStorageS3(fs)
	// A Redis with one heartbeat: saving a host-path backend now consults the
	// online-Core count, and refuses when it cannot be taken at all.
	h := NewCoreStorageHandler(&AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")})

	dir := testConnectionProbeDir(t)
	body, _ := json.Marshal(CoreStorageConfig{Backend: "path", Path: dir, PathConfirmed: true})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[keyCoreStorageS3SecretKey] != "" {
		t.Errorf("stored s3 secret not cleared after switching to path backend, got %q", fs.kv[keyCoreStorageS3SecretKey])
	}
}

func TestCoreStorage_SaveRejectsInvalid(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	h := NewCoreStorageHandler(&AppState{Store: fs})
	body, _ := json.Marshal(CoreStorageConfig{Backend: "path", Path: "relative", PathConfirmed: true})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("SaveConfig invalid status = %d, want 400", rw.Code)
	}
	if fs.kv[keyCoreStorageBackend] != "" {
		t.Errorf("invalid config was partially persisted: %v", fs.kv)
	}
}

// TestCoreStorage_SaveRejectsInvalid_DoesNotWipeExistingValidConfig guards the
// "reject before persist" contract from the other direction: an admin who
// already has a valid config saved and submits a bad edit must not lose the
// previously-good settings.
func TestCoreStorage_SaveRejectsInvalid_DoesNotWipeExistingValidConfig(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	fs.kv[keyCoreStorageBackend] = "path"
	fs.kv[keyCoreStoragePath] = "/mnt/shared"
	fs.kv[keyCoreStoragePathConfirm] = "true"
	h := NewCoreStorageHandler(&AppState{Store: fs})

	body, _ := json.Marshal(CoreStorageConfig{Backend: "path", Path: "relative", PathConfirmed: true})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rw.Code)
	}
	if fs.kv[keyCoreStoragePath] != "/mnt/shared" {
		t.Errorf("invalid save clobbered the existing good path, got %q", fs.kv[keyCoreStoragePath])
	}
}

func TestCoreStorage_GetConfig_NeverConfigured(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	h := NewCoreStorageHandler(&AppState{Store: fs})
	rw := httptest.NewRecorder()
	h.GetConfig(rw, httptest.NewRequest(http.MethodGet, "/api/settings/core-storage", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	var got struct {
		Settings CoreStorageConfig `json:"settings"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &got)
	if got.Settings.S3SecretSet {
		t.Errorf("S3SecretSet = true on a never-configured store, want false")
	}
}

// testConnectionProbeDir returns an absolute-looking POSIX path ("/...")
// that os.MkdirAll can still create on a Windows dev box (Windows treats a
// leading "/" as drive-root-relative), matching validateCoreStorageConfig's
// deliberately Linux-only absolute-path check (see its doc comment). Same
// convention as the "/mnt/shared" literal already used in
// core_storage_builder_test.go; a real t.TempDir() would fail the "/" check
// on Windows and was deliberately not used here for that reason.
func testConnectionProbeDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join("/", "dylaris-test-tmp")
	dir := filepath.ToSlash(filepath.Join(root, t.Name()))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return dir
}

func TestCoreStorage_TestConnection_PathProbe(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	dir := testConnectionProbeDir(t)
	fs.kv[keyCoreStorageBackend] = "path"
	fs.kv[keyCoreStoragePath] = dir
	fs.kv[keyCoreStoragePathConfirm] = "true"
	h := NewCoreStorageHandler(&AppState{Store: fs})

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
	h := NewCoreStorageHandler(&AppState{Store: fs})

	body, _ := json.Marshal(CoreStorageConfig{Backend: "path", Path: dir, PathConfirmed: true})
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
	h := NewCoreStorageHandler(&AppState{Store: fs})
	body, _ := json.Marshal(CoreStorageConfig{Backend: "path", Path: "relative", PathConfirmed: true})
	rw := httptest.NewRecorder()
	h.TestConnection(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage/test", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (invalid candidate reports ok:false in-band)", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "\"ok\":false") {
		t.Errorf("body = %s, want ok:false", rw.Body.String())
	}
}

// --- mergeCoreStorageCandidate (pure logic backing both SaveConfig and
// TestConnection's "blank secret keeps the stored one" rule) ---

func TestMergeCoreStorageCandidate(t *testing.T) {
	existing := CoreStorageConfig{
		Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k",
		S3SecretKey: "stored-secret",
	}

	cases := []struct {
		name      string
		candidate CoreStorageConfig
		want      CoreStorageConfig
	}{
		{
			name:      "empty candidate backend reuses the stored config entirely",
			candidate: CoreStorageConfig{},
			want:      existing,
		},
		{
			// Fix 1: identity fields (endpoint/bucket/accessKey) all match the
			// stored config, so a blank secret is safe to backfill.
			name: "blank secret + unchanged endpoint/bucket/accessKey reuses the stored secret",
			candidate: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k",
			},
			want: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k",
				S3SecretKey: "stored-secret",
			},
		},
		{
			// Fix 1 (IMPORTANT): a changed bucket must NOT be silently paired
			// with the old secret - that is a credential-rebinding gap, and it
			// also produces a broken config with no error.
			name: "blank secret + changed bucket does not backfill",
			candidate: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "new", S3AccessKey: "k",
			},
			want: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "new", S3AccessKey: "k",
			},
		},
		{
			name: "blank secret + changed access key does not backfill",
			candidate: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k2",
			},
			want: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k2",
			},
		},
		{
			name: "blank secret + changed endpoint does not backfill",
			candidate: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "old", S3AccessKey: "k",
			},
			want: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "old", S3AccessKey: "k",
			},
		},
		{
			name: "candidate with its own secret is not overridden",
			candidate: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "new", S3AccessKey: "k2",
				S3SecretKey: "new-secret",
			},
			want: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "new", S3AccessKey: "k2",
				S3SecretKey: "new-secret",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeCoreStorageCandidate(c.candidate, existing)
			if got != c.want {
				t.Errorf("mergeCoreStorageCandidate() = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestNormalizeCoreStorageCandidate(t *testing.T) {
	got := normalizeCoreStorageCandidate(CoreStorageConfig{
		Backend: " s3 ", Path: " /mnt/x ", S3Endpoint: " https://e ", S3Bucket: " b ",
		S3AccessKey: " k ", S3SecretKey: " s ",
	})
	want := CoreStorageConfig{
		Backend: "s3", Path: "/mnt/x", S3Endpoint: "https://e", S3Bucket: "b",
		S3AccessKey: "k", S3SecretKey: "s",
	}
	if got != want {
		t.Errorf("normalizeCoreStorageCandidate() = %+v, want %+v", got, want)
	}
}

// --- Fix 1 (credential-rebinding gap): a blank secret must only backfill the
// stored secret when the identity-defining fields (s3Endpoint/s3Bucket/
// s3AccessKey) are unchanged from the stored config. Otherwise the request
// must be rejected instead of silently pairing the real secret with a new
// identity. Covered for both SaveConfig (persists) and TestConnection
// (probes) since mergeCoreStorageCandidate backs both. ---

func seedCoreStorageS3(fs *coreStorageHTTPFakeStore) {
	fs.kv[keyCoreStorageBackend] = "s3"
	fs.kv[keyCoreStorageS3Endpoint] = "https://s3.example.com"
	fs.kv[keyCoreStorageS3Bucket] = "bucket1"
	fs.kv[keyCoreStorageS3AccessKey] = "key1"
	fs.kv[keyCoreStorageS3SecretKey] = "stored-secret"
}

func TestCoreStorage_SaveConfig_BlankSecretUnchangedIdentity_ReusesStoredSecret(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	seedCoreStorageS3(fs)
	h := NewCoreStorageHandler(&AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")})

	body, _ := json.Marshal(CoreStorageConfig{
		Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "bucket1", S3AccessKey: "key1",
		S3Region: "eu-west-1", // an unrelated field changes, proving the save actually went through
	})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[keyCoreStorageS3SecretKey] != "stored-secret" {
		t.Errorf("stored secret not reused, got %q", fs.kv[keyCoreStorageS3SecretKey])
	}
	if fs.kv[keyCoreStorageS3Region] != "eu-west-1" {
		t.Errorf("unrelated field did not persist, got %q", fs.kv[keyCoreStorageS3Region])
	}
}

func TestCoreStorage_SaveConfig_BlankSecretChangedAccessKey_Rejected(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	seedCoreStorageS3(fs)
	h := NewCoreStorageHandler(&AppState{Store: fs})

	body, _ := json.Marshal(CoreStorageConfig{
		Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "bucket1", S3AccessKey: "key2",
	})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (changed access key + blank secret must be rejected); body=%s", rw.Code, rw.Body.String())
	}
	if fs.kv[keyCoreStorageS3AccessKey] != "key1" {
		t.Errorf("rejected save persisted the new access key anyway, got %q", fs.kv[keyCoreStorageS3AccessKey])
	}
	if fs.kv[keyCoreStorageS3SecretKey] != "stored-secret" {
		t.Errorf("rejected save touched the stored secret, got %q", fs.kv[keyCoreStorageS3SecretKey])
	}
}

func TestCoreStorage_SaveConfig_BlankSecretChangedEndpoint_Rejected(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	seedCoreStorageS3(fs)
	h := NewCoreStorageHandler(&AppState{Store: fs})

	body, _ := json.Marshal(CoreStorageConfig{
		Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "bucket1", S3AccessKey: "key1",
	})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (changed endpoint + blank secret must be rejected); body=%s", rw.Code, rw.Body.String())
	}
	if fs.kv[keyCoreStorageS3Endpoint] != "https://s3.example.com" {
		t.Errorf("rejected save persisted the new endpoint anyway, got %q", fs.kv[keyCoreStorageS3Endpoint])
	}
}

func TestCoreStorage_TestConnection_BlankSecretChangedAccessKey_Rejected(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	seedCoreStorageS3(fs)
	h := NewCoreStorageHandler(&AppState{Store: fs})

	body, _ := json.Marshal(CoreStorageConfig{
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
	h := NewCoreStorageHandler(&AppState{Store: fs})

	body, _ := json.Marshal(CoreStorageConfig{
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

// disableAWSRetries forces the AWS SDK to attempt exactly once instead of
// its default 3-attempt retry-with-backoff. Port 1 on 127.0.0.1 refuses the
// connection instantly (no DNS lookup, no listener), but the SDK's default
// retryer still treats a connection-refused dial error as retryable and
// sleeps out an exponential backoff between attempts, which is what made
// these tests take several seconds each despite already targeting loopback.
// AWS_MAX_ATTEMPTS is read by awsconfig.LoadDefaultConfig (used in
// storage/backup/s3.go, a production file this fix does not touch), so
// setting it here is a test-only way to get a single, immediate attempt.
func disableAWSRetries(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
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
	fs.kv[keyCoreStorageBackend] = "s3"
	fs.kv[keyCoreStorageS3Endpoint] = "http://127.0.0.1:1"
	fs.kv[keyCoreStorageS3Bucket] = "bucket1"
	fs.kv[keyCoreStorageS3AccessKey] = "key1"
	fs.kv[keyCoreStorageS3SecretKey] = "stored-secret"
	h := NewCoreStorageHandler(&AppState{Store: fs})

	body, _ := json.Marshal(CoreStorageConfig{
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
	h := NewCoreStorageHandler(&AppState{Store: fs})

	const theSecret = "unleaked-topsecret-value"
	body, _ := json.Marshal(CoreStorageConfig{
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

// TestCoreStorage_SaveConfig_TrimsS3FieldWhitespace guards Fix 2: a
// clipboard-pasted key/secret/endpoint/bucket with surrounding whitespace
// must be trimmed before persisting, not silently break auth later.
func TestCoreStorage_SaveConfig_TrimsS3FieldWhitespace(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	h := NewCoreStorageHandler(&AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")})

	body, _ := json.Marshal(CoreStorageConfig{
		Backend: "s3", S3Endpoint: "  https://s3.example.com  ", S3Bucket: "  bucket1 ",
		S3AccessKey: " key1 ", S3SecretKey: " sekret ",
	})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[keyCoreStorageS3Endpoint] != "https://s3.example.com" {
		t.Errorf("s3Endpoint not trimmed, got %q", fs.kv[keyCoreStorageS3Endpoint])
	}
	if fs.kv[keyCoreStorageS3Bucket] != "bucket1" {
		t.Errorf("s3Bucket not trimmed, got %q", fs.kv[keyCoreStorageS3Bucket])
	}
	if fs.kv[keyCoreStorageS3AccessKey] != "key1" {
		t.Errorf("s3AccessKey not trimmed, got %q", fs.kv[keyCoreStorageS3AccessKey])
	}
	if fs.kv[keyCoreStorageS3SecretKey] != "sekret" {
		t.Errorf("s3SecretKey not trimmed, got %q", fs.kv[keyCoreStorageS3SecretKey])
	}
}

// TestCoreStorage_TestConnection_PathProbe_CleansUpProbeDir guards Fix 4: a
// tested (whether saved or not) path backend must not leave an empty
// "_probe" directory behind forever.
func TestCoreStorage_TestConnection_PathProbe_CleansUpProbeDir(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	dir := testConnectionProbeDir(t)
	fs.kv[keyCoreStorageBackend] = "path"
	fs.kv[keyCoreStoragePath] = dir
	fs.kv[keyCoreStoragePathConfirm] = "true"
	h := NewCoreStorageHandler(&AppState{Store: fs})

	rw := httptest.NewRecorder()
	h.TestConnection(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage/test", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("TestConnection status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "_probe")); !os.IsNotExist(err) {
		t.Errorf("_probe directory left behind after test-connection, stat err = %v", err)
	}
}

// --- probeStorageProvider (write+read+delete probe logic), exercised
// against a hand-written fake so the mismatch/error cleanup paths are
// deterministic and do not depend on a real filesystem or network backend. ---

type fakeProbeProvider struct {
	readBack     string
	getErr       error
	writeErr     error
	deleteErr    error
	deleteCalled int
}

func (f *fakeProbeProvider) ListFiles(context.Context, string) ([]storage.FileInfo, error) {
	return nil, nil
}
func (f *fakeProbeProvider) CreateDir(context.Context, string) error           { return nil }
func (f *fakeProbeProvider) CopyToLocal(context.Context, string, string) error { return nil }
func (f *fakeProbeProvider) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func (f *fakeProbeProvider) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}
func (f *fakeProbeProvider) WriteFile(context.Context, string, io.Reader) error {
	return f.writeErr
}

func (f *fakeProbeProvider) GetFile(context.Context, string) (io.ReadCloser, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return io.NopCloser(strings.NewReader(f.readBack)), nil
}

func (f *fakeProbeProvider) DeletePath(context.Context, string) error {
	f.deleteCalled++
	return f.deleteErr
}

func TestProbeStorageProvider_Success(t *testing.T) {
	fp := &fakeProbeProvider{readBack: coreStorageProbePayload}
	ok, msg := probeStorageProvider(context.Background(), fp)
	if !ok {
		t.Fatalf("ok = false, want true (%s)", msg)
	}
	if fp.deleteCalled != 1 {
		t.Errorf("DeletePath called %d times, want 1", fp.deleteCalled)
	}
}

// TestProbeStorageProvider_WriteFailureCleansUpProbe guards Fix 3: the
// write-failure path must attempt cleanup like every other return path,
// because a backend is not required to leave nothing behind after a failed
// write. LocalProvider happens to be tidy about it now (it stages and
// renames); the probe does not get to assume that of the provider it was
// handed.
func TestProbeStorageProvider_WriteFailureCleansUpProbe(t *testing.T) {
	fp := &fakeProbeProvider{writeErr: errors.New("disk full")}
	ok, msg := probeStorageProvider(context.Background(), fp)
	if ok {
		t.Fatalf("ok = true, want false on write failure (%s)", msg)
	}
	if fp.deleteCalled != 1 {
		t.Errorf("DeletePath called %d times on write failure, want 1 (a failed write can leave a truncated object behind)", fp.deleteCalled)
	}
}

func TestProbeStorageProvider_MismatchStillDeletesProbe(t *testing.T) {
	fp := &fakeProbeProvider{readBack: "not-what-was-written"}
	ok, msg := probeStorageProvider(context.Background(), fp)
	if ok {
		t.Fatalf("ok = true, want false on mismatch (%s)", msg)
	}
	if fp.deleteCalled != 1 {
		t.Errorf("DeletePath called %d times on mismatch, want 1 (probe must still be cleaned up)", fp.deleteCalled)
	}
}

func TestProbeStorageProvider_ReadErrorStillDeletesProbe(t *testing.T) {
	fp := &fakeProbeProvider{getErr: errors.New("boom")}
	ok, msg := probeStorageProvider(context.Background(), fp)
	if ok {
		t.Fatalf("ok = true, want false on read error (%s)", msg)
	}
	if fp.deleteCalled != 1 {
		t.Errorf("DeletePath called %d times on read error, want 1", fp.deleteCalled)
	}
}

func TestProbeStorageProvider_DeleteFailureAfterMatchReportsFailure(t *testing.T) {
	fp := &fakeProbeProvider{readBack: coreStorageProbePayload, deleteErr: errors.New("locked")}
	ok, msg := probeStorageProvider(context.Background(), fp)
	if ok {
		t.Fatalf("ok = true, want false when cleanup delete fails (%s)", msg)
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
			h := NewCoreStorageHandler(&AppState{Store: fs})
			var body []byte
			if c.backend == "s3" {
				// A loopback port nothing listens on, the same shape the other
				// s3 tests here use. The probe fails, which is fine: the point
				// is that an s3 config has no local path, so the check must not
				// be consulted at all - asserted via `called` below.
				disableAWSRetries(t)
				body, _ = json.Marshal(CoreStorageConfig{
					Backend: "s3", S3Endpoint: "http://127.0.0.1:1", S3Bucket: "b",
					S3AccessKey: "k", S3SecretKey: "s",
				})
			} else {
				body, _ = json.Marshal(CoreStorageConfig{
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
