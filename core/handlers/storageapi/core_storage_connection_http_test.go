package storageapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
)

// coreStorageConnFakeStore adds a storage-connection to the read/write settings
// fake, so a SaveConfig with a selected connection can be exercised end to end.
type coreStorageConnFakeStore struct {
	*coreStorageHTTPFakeStore
	conn    *models.StorageConnection
	connErr error
}

func (f *coreStorageConnFakeStore) GetStorageConnection(int) (*models.StorageConnection, error) {
	if f.connErr != nil {
		return nil, f.connErr
	}
	return f.conn, nil
}

func newCoreStorageConnFakeStore(conn *models.StorageConnection, connErr error) *coreStorageConnFakeStore {
	return &coreStorageConnFakeStore{
		coreStorageHTTPFakeStore: newCoreStorageHTTPFakeStore(),
		conn:                     conn,
		connErr:                  connErr,
	}
}

func validConnection() *models.StorageConnection {
	cfg, _ := json.Marshal(handlers.StorageConnectionConfig{Endpoint: "https://s3.example", Region: "eu", Bucket: "conn-bucket"})
	return &models.StorageConnection{ID: 5, Name: "nas", Provider: "s3", Config: cfg, AccessKey: "CAK", SecretAccessKey: "CSK"}
}

// TestSaveConfig_ConnectionValidatesConnectionNotInline is the core guard: with
// a connection selected, blank inline s3 fields must NOT fail validation - the
// connection supplies the credentials.
func TestSaveConfig_ConnectionValidatesConnectionNotInline(t *testing.T) {
	fs := newCoreStorageConnFakeStore(validConnection(), nil)
	// A single online Core so checkSharedStorageReachable's short-circuit
	// applies; this test is about the merge/validation behaviour, not the
	// reachability round.
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")})

	// backend s3 + a connection, but the inline s3 fields are all blank.
	body, _ := json.Marshal(handlers.CoreStorageConfig{Backend: "s3", ConnectionID: 5})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))

	if rw.Code != http.StatusOK {
		t.Fatalf("SaveConfig status = %d, want 200 (blank inline must be fine with a connection) (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[handlers.KeyCoreStorageConnectionID] != "5" {
		t.Errorf("connection id not persisted, got %q", fs.kv[handlers.KeyCoreStorageConnectionID])
	}
}

// TestSaveConfig_BlankBackendWithConnectionStillSaves proves the implied-backend
// line: a connection selection with no backend field must not be dropped by the
// merge.
func TestSaveConfig_BlankBackendWithConnectionStillSaves(t *testing.T) {
	fs := newCoreStorageConnFakeStore(validConnection(), nil)
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")})

	body, _ := json.Marshal(handlers.CoreStorageConfig{ConnectionID: 5}) // no Backend
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))

	if rw.Code != http.StatusOK {
		t.Fatalf("SaveConfig status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[handlers.KeyCoreStorageConnectionID] != "5" {
		t.Errorf("connection id not persisted with a blank backend, got %q", fs.kv[handlers.KeyCoreStorageConnectionID])
	}
}

func TestSaveConfig_MissingConnectionRejected(t *testing.T) {
	fs := newCoreStorageConnFakeStore(nil, errors.New("no such row"))
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})

	body, _ := json.Marshal(handlers.CoreStorageConfig{Backend: "s3", ConnectionID: 9})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("SaveConfig status = %d, want 400 for a missing connection", rw.Code)
	}
	if fs.kv[handlers.KeyCoreStorageConnectionID] != "" {
		t.Errorf("a broken connection selection was persisted: %q", fs.kv[handlers.KeyCoreStorageConnectionID])
	}
}

func TestSaveConfig_ConnectionWithoutSecretRejected(t *testing.T) {
	conn := validConnection()
	conn.SecretAccessKey = "" // e.g. secret was never set, or decrypt failed
	fs := newCoreStorageConnFakeStore(conn, nil)
	h := NewCoreStorageHandler(&handlers.AppState{Store: fs})

	body, _ := json.Marshal(handlers.CoreStorageConfig{Backend: "s3", ConnectionID: 5})
	rw := httptest.NewRecorder()
	h.SaveConfig(rw, httptest.NewRequest(http.MethodPost, "/api/settings/core-storage", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("SaveConfig status = %d, want 400 for a connection with no secret (%s)", rw.Code, rw.Body.String())
	}
}
