package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// backupValidationFakeStore records whether the store was ever asked to
// persist, so a rejected request can be proven not to have reached it. Local
// to this file; never redeclared.
type backupValidationFakeStore struct {
	store.Store
	created int
	updated int
}

func (f *backupValidationFakeStore) CreateBackupStorage(*models.BackupStorage) (int, error) {
	f.created++
	return 1, nil
}
func (f *backupValidationFakeStore) UpdateBackupStorage(*models.BackupStorage) error {
	f.updated++
	return nil
}

// GetBackupStorage answers UpdateStorage's platform-row check and secret
// backfill. This test is about provider validation, so it is a platform row
// holding no secret, which mergeBackupStorageSecret leaves untouched.
func (f *backupValidationFakeStore) GetBackupStorage(id int) (*models.BackupStorage, error) {
	return &models.BackupStorage{ID: id}, nil
}

func TestValidBackupProvider(t *testing.T) {
	cases := []struct {
		provider string
		want     bool
	}{
		{"local", true},
		{"shared", true},
		{"s3", true},
		{"node-local", true},
		{"core-storage", true},
		{"connection", true},
		{"", false},
		{"core storage", false},
		{"corestorage", false},
		{"core-storag", false},
		{"S3", false},
		{"ftp", false},
	}
	for _, c := range cases {
		t.Run("provider="+c.provider, func(t *testing.T) {
			if got := validBackupProvider(c.provider); got != c.want {
				t.Errorf("validBackupProvider(%q) = %v, want %v", c.provider, got, c.want)
			}
		})
	}
}

// TestValidBackupMode's brief-specified cases (including "core-storage" and
// "core storage") are merged into the pre-existing TestValidBackupMode in
// settings_validators_test.go instead of being redeclared here - Go does not
// allow two top-level funcs with the same name in one package, and that test
// already covers validBackupMode.

func TestCreateStorage_RejectsUnknownProviderBeforePersisting(t *testing.T) {
	fs := &backupValidationFakeStore{}
	h := NewBackupHandler(&AppState{Store: fs})

	body, _ := json.Marshal(models.BackupStorage{Name: "typo", Provider: "corestorage"})
	rw := httptest.NewRecorder()
	h.CreateStorage(rw, httptest.NewRequest(http.MethodPost, "/api/backup-storages", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("CreateStorage status = %d, want 400 (%s)", rw.Code, rw.Body.String())
	}
	if fs.created != 0 {
		t.Errorf("a typo provider was persisted (%d creates); it must be rejected at the boundary", fs.created)
	}
}

func TestCreateStorage_AcceptsCoreStorage(t *testing.T) {
	fs := &backupValidationFakeStore{}
	h := NewBackupHandler(&AppState{Store: fs})

	body, _ := json.Marshal(models.BackupStorage{Name: "Core storage", Provider: "core-storage"})
	rw := httptest.NewRecorder()
	h.CreateStorage(rw, httptest.NewRequest(http.MethodPost, "/api/backup-storages", bytes.NewReader(body)))

	if rw.Code != http.StatusOK {
		t.Fatalf("CreateStorage status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.created != 1 {
		t.Errorf("core-storage was not persisted (%d creates)", fs.created)
	}
}

func TestUpdateStorage_RejectsUnknownProviderBeforePersisting(t *testing.T) {
	fs := &backupValidationFakeStore{}
	h := NewBackupHandler(&AppState{Store: fs})

	body, _ := json.Marshal(models.BackupStorage{Name: "x", Provider: "ftp"})
	req := httptest.NewRequest(http.MethodPatch, "/api/backup-storages/1", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": "1"})
	rw := httptest.NewRecorder()
	h.UpdateStorage(rw, req)

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("UpdateStorage status = %d, want 400 (%s)", rw.Code, rw.Body.String())
	}
	if fs.updated != 0 {
		t.Errorf("a typo provider was persisted on update (%d updates)", fs.updated)
	}
}

func TestUpdateStorage_AcceptsCoreStorage(t *testing.T) {
	fs := &backupValidationFakeStore{}
	h := NewBackupHandler(&AppState{Store: fs})

	body, _ := json.Marshal(models.BackupStorage{Name: "x", Provider: "core-storage"})
	req := httptest.NewRequest(http.MethodPatch, "/api/backup-storages/1", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": "1"})
	rw := httptest.NewRecorder()
	h.UpdateStorage(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("UpdateStorage status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.updated != 1 {
		t.Errorf("core-storage update was not persisted (%d updates)", fs.updated)
	}
}

func TestCreateStorage_AcceptsConnection(t *testing.T) {
	fs := &backupValidationFakeStore{}
	h := NewBackupHandler(&AppState{Store: fs})

	body, _ := json.Marshal(models.BackupStorage{
		Name:     "R2 backups",
		Provider: "connection",
		Config:   json.RawMessage(`{"connectionId":3,"prefix":"server-backups"}`),
	})
	rw := httptest.NewRecorder()
	h.CreateStorage(rw, httptest.NewRequest(http.MethodPost, "/api/backup-storages", bytes.NewReader(body)))

	if rw.Code != http.StatusOK {
		t.Fatalf("CreateStorage status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.created != 1 {
		t.Errorf("connection provider was not persisted (%d creates)", fs.created)
	}
}
