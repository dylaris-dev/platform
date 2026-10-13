package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

func s3Storage(secret string) models.BackupStorage {
	cfg, _ := json.Marshal(map[string]string{
		"endpoint": "https://objects.internal:9000", "bucket": "backups",
		"region": "us-east-1", "accessKeyId": "AKIA", "secretAccessKey": secret,
	})
	return models.BackupStorage{ID: 1, Name: "s3", Provider: "s3", Config: cfg}
}

// TestRedactBackupStorageSecret_RemovesTheSecret is the leak fix: the read path
// must never carry secretAccessKey, and it must flag that one exists so the
// panel can show a write-only field.
func TestRedactBackupStorageSecret_RemovesTheSecret(t *testing.T) {
	got := redactBackupStorageSecret(s3Storage("super-secret-value"))

	if strings.Contains(string(got.Config), "super-secret-value") {
		t.Fatalf("redacted config still contains the secret: %s", got.Config)
	}
	if strings.Contains(string(got.Config), "secretAccessKey") {
		t.Errorf("redacted config still names secretAccessKey: %s", got.Config)
	}
	if !got.SecretSet {
		t.Error("SecretSet = false, want true when a secret was stored")
	}
	// The non-secret fields survive so the form can still show them.
	if !strings.Contains(string(got.Config), "backups") {
		t.Errorf("redaction dropped a non-secret field: %s", got.Config)
	}
}

func TestRedactBackupStorageSecret_NoSecretStored(t *testing.T) {
	got := redactBackupStorageSecret(s3Storage(""))
	if got.SecretSet {
		t.Error("SecretSet = true, want false when the stored secret is empty")
	}
}

// TestRedactBackupStorageSecret_NonS3Unchanged: only s3 carries a secret; a
// local/node-local config is returned as-is.
func TestRedactBackupStorageSecret_NonS3Unchanged(t *testing.T) {
	bs := models.BackupStorage{Provider: "local", Config: json.RawMessage(`{"basePath":"/data"}`)}
	got := redactBackupStorageSecret(bs)
	if string(got.Config) != `{"basePath":"/data"}` || got.SecretSet {
		t.Errorf("non-s3 storage was altered: %+v", got)
	}
}

// TestRedactBackupStorageSecret_UnparseableConfigDropsTheBlob: a config that
// cannot be parsed field-by-field is emptied rather than risk leaking part of
// a secret it might contain.
func TestRedactBackupStorageSecret_UnparseableConfigDropsTheBlob(t *testing.T) {
	bs := models.BackupStorage{Provider: "s3", Config: json.RawMessage(`not json`)}
	got := redactBackupStorageSecret(bs)
	if string(got.Config) != "{}" {
		t.Errorf("config = %s, want {} for an unparseable s3 config", got.Config)
	}
}

// TestMergeBackupStorageSecret_KeepsExistingWhenBlank is what makes the
// write-only field usable: an edit with no secret preserves the stored one.
func TestMergeBackupStorageSecret_KeepsExistingWhenBlank(t *testing.T) {
	existing := s3Storage("stored-secret")
	incoming := s3Storage("") // panel sent a blank secret

	merged, err := mergeBackupStorageSecret(incoming, &existing)
	if err != nil {
		t.Fatalf("blank edit with an unchanged identity was refused: %v", err)
	}

	if !strings.Contains(string(merged.Config), "stored-secret") {
		t.Fatalf("blank edit wiped the secret: %s", merged.Config)
	}
}

// TestMergeBackupStorageSecret_UsesNewSecret: a submitted secret is a rotation
// and must replace the stored one.
func TestMergeBackupStorageSecret_UsesNewSecret(t *testing.T) {
	existing := s3Storage("old-secret")
	incoming := s3Storage("new-secret")

	merged, err := mergeBackupStorageSecret(incoming, &existing)
	if err != nil {
		t.Fatalf("rotation was refused: %v", err)
	}

	if !strings.Contains(string(merged.Config), "new-secret") {
		t.Fatalf("rotation was ignored: %s", merged.Config)
	}
	if strings.Contains(string(merged.Config), "old-secret") {
		t.Errorf("both secrets present after rotation: %s", merged.Config)
	}
}

// backupSecretFakeStore serves a canned storage list and records updates, so a
// handler-level test can prove the redaction/merge actually run on the wire.
type backupSecretFakeStore struct {
	store.Store
	list    []models.BackupStorage
	byID    map[int]*models.BackupStorage
	updated *models.BackupStorage
}

func (f *backupSecretFakeStore) ListBackupStorages() ([]models.BackupStorage, error) {
	return f.list, nil
}
func (f *backupSecretFakeStore) GetBackupStorage(id int) (*models.BackupStorage, error) {
	return f.byID[id], nil
}
func (f *backupSecretFakeStore) UpdateBackupStorage(s *models.BackupStorage) error {
	f.updated = s
	return nil
}

// TestListStorages_NeverReturnsTheSecret is the leak fix on the wire: the
// response to GET /api/backup-storages must not contain the s3 secret, whatever
// a settings.read holder does with it.
func TestListStorages_NeverReturnsTheSecret(t *testing.T) {
	st := &backupSecretFakeStore{list: []models.BackupStorage{s3Storage("leaked-secret-abc")}}
	h := &BackupHandler{state: &AppState{Store: st}}

	rec := httptest.NewRecorder()
	h.ListStorages(rec, httptest.NewRequest(http.MethodGet, "/api/backup-storages", nil))

	body := rec.Body.String()
	if strings.Contains(body, "leaked-secret-abc") {
		t.Fatalf("the list response leaked the s3 secret: %s", body)
	}
	if !strings.Contains(body, "secretSet") {
		t.Errorf("response omits secretSet, so the panel cannot show a write-only field: %s", body)
	}
}

// TestUpdateStorage_BlankSecretKeepsTheStoredOne pins the write side end to end:
// an update carrying a blank secret must persist the existing one, not wipe it.
func TestUpdateStorage_BlankSecretKeepsTheStoredOne(t *testing.T) {
	existing := s3Storage("stored-secret-xyz")
	st := &backupSecretFakeStore{byID: map[int]*models.BackupStorage{1: &existing}}
	h := &BackupHandler{state: &AppState{Store: st}}

	// The panel re-submits the redacted config (no secret).
	blank := s3Storage("")
	blank.ID = 1
	payload, _ := json.Marshal(blank)
	req := httptest.NewRequest(http.MethodPatch, "/api/backup-storages/1", strings.NewReader(string(payload)))
	req = mux.SetURLVars(req, map[string]string{"id": "1"})

	h.UpdateStorage(httptest.NewRecorder(), req)

	if st.updated == nil {
		t.Fatal("UpdateBackupStorage was never called")
	}
	if !strings.Contains(string(st.updated.Config), "stored-secret-xyz") {
		t.Fatalf("blank update wiped the stored secret: %s", st.updated.Config)
	}
}

// s3StorageAt is s3Storage with the identity fields under the caller's control,
// so a test can express "same credential, different destination".
func s3StorageAt(endpoint, bucket, accessKey, secret string) models.BackupStorage {
	cfg, _ := json.Marshal(map[string]string{
		"endpoint": endpoint, "bucket": bucket,
		"region": "us-east-1", "accessKeyId": accessKey, "secretAccessKey": secret,
	})
	return models.BackupStorage{ID: 1, Name: "s3", Provider: "s3", Config: cfg}
}

// TestMergeBackupStorageSecret_RefusesToRebindTheStoredSecret is the security
// half of the write-only field, and the reason mergeCoreStorageCandidate has
// always compared these three fields.
//
// settings.write is delegatable and ListStorages redacts the secret on every
// read, so without this a holder who cannot READ the secret redirects it simply
// by submitting a different endpoint or bucket with the secret left blank:
// SigV4 signs with it rather than sending it, so an attacker-chosen host starts
// receiving validly signed requests for the operator's backups.
func TestMergeBackupStorageSecret_RefusesToRebindTheStoredSecret(t *testing.T) {
	existing := s3StorageAt("https://objects.internal:9000", "backups", "AKIA", "stored-secret")

	for _, tc := range []struct {
		name     string
		incoming models.BackupStorage
	}{
		{"endpoint redirected", s3StorageAt("https://attacker.example", "backups", "AKIA", "")},
		{"bucket changed", s3StorageAt("https://objects.internal:9000", "other-bucket", "AKIA", "")},
		{"access key changed", s3StorageAt("https://objects.internal:9000", "backups", "AKIB", "")},
		{"identity fields omitted entirely", models.BackupStorage{Provider: "s3", Config: json.RawMessage(`{}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := mergeBackupStorageSecret(tc.incoming, &existing)
			if !errors.Is(err, ErrBackupStorageSecretRequired) {
				t.Fatalf("err = %v, want ErrBackupStorageSecretRequired", err)
			}
			// And it must not have leaked the stored secret into the returned row
			// on the way out, which would persist the rebinding anyway.
			if strings.Contains(string(merged.Config), "stored-secret") {
				t.Fatalf("the refused merge still carried the stored secret: %s", merged.Config)
			}
		})
	}
}

// A row with no secret stored has nothing to rebind and nothing to wipe, so an
// identity change there is an ordinary edit and must not be refused.
func TestMergeBackupStorageSecret_NoStoredSecretIsNotRefused(t *testing.T) {
	existing := s3StorageAt("https://objects.internal:9000", "backups", "AKIA", "")
	incoming := s3StorageAt("https://elsewhere.example", "other", "AKIB", "")

	if _, err := mergeBackupStorageSecret(incoming, &existing); err != nil {
		t.Fatalf("an edit with nothing to protect was refused: %v", err)
	}
}

// TestValidateBackupStorageEndpoint covers the check backup_storages was the
// last s3 surface to skip. It fails CLOSED on "@": a valid s3 endpoint host
// cannot contain one, and a password with a space defeats url.Parse, so
// "parses cleanly" is not a safe acceptance test.
func TestValidateBackupStorageEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bs      models.BackupStorage
		wantErr bool
	}{
		{"credentialed endpoint", s3StorageAt("https://AKIA:secret@objects.internal", "b", "AKIA", "s"), true},
		{"plain endpoint", s3StorageAt("https://objects.internal:9000", "b", "AKIA", "s"), false},
		{"empty endpoint is the AWS default", s3StorageAt("", "b", "AKIA", "s"), false},
		{"non-s3 provider has no endpoint", models.BackupStorage{Provider: "node-local"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBackupStorageEndpoint(tc.bs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateBackupStorageEndpoint err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestUpdateStorage_RefusesARebindOnTheWire is the call-site half: the merge
// refusing is worthless if the handler ignores the error and writes the row
// anyway. It must answer 400 and persist NOTHING - a silent save would drop the
// credential and report success.
func TestUpdateStorage_RefusesARebindOnTheWire(t *testing.T) {
	existing := s3StorageAt("https://objects.internal:9000", "backups", "AKIA", "stored-secret-xyz")
	st := &backupSecretFakeStore{byID: map[int]*models.BackupStorage{1: &existing}}
	h := &BackupHandler{state: &AppState{Store: st}}

	redirected := s3StorageAt("https://attacker.example", "backups", "AKIA", "")
	redirected.ID = 1
	payload, _ := json.Marshal(redirected)
	req := httptest.NewRequest(http.MethodPatch, "/api/backup-storages/1", strings.NewReader(string(payload)))
	req = mux.SetURLVars(req, map[string]string{"id": "1"})

	rec := httptest.NewRecorder()
	h.UpdateStorage(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if st.updated != nil {
		t.Fatalf("the refused edit was persisted anyway: %s", st.updated.Config)
	}
}

// A credentialed endpoint must be refused on the write path too, not merely by
// the helper.
func TestCreateStorage_RefusesACredentialedEndpoint(t *testing.T) {
	st := &backupSecretFakeStore{}
	h := &BackupHandler{state: &AppState{Store: st}}

	payload, _ := json.Marshal(s3StorageAt("https://AKIA:secret@objects.internal", "backups", "AKIA", "s"))
	req := httptest.NewRequest(http.MethodPost, "/api/backup-storages", strings.NewReader(string(payload)))

	rec := httptest.NewRecorder()
	h.CreateStorage(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func (f *backupSecretFakeStore) BackupStorageInUse(int) (bool, error) { return false, nil }
