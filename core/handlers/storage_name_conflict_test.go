package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// Every other named resource in the API answers a duplicate name with 409 and a
// sentence the panel can show (panel roles, server roles, nodes, packs, ticket
// categories, canned responses, pack builds). The two storage endpoints echoed
// the driver instead: 500 with `pq: duplicate key value violates unique
// constraint "backup_storages_name_key" (23505)`, which is both the wrong class
// and a needless disclosure of the schema. Live-reproduced on the testbed.
//
// A PATCH against an id that does not exist answered 200 success for the same
// reason - nothing looked at RowsAffected.

// nameConflictStore returns the sentinels the real store now returns.
type nameConflictStore struct {
	store.Store
	err error
}

func (f *nameConflictStore) CreateBackupStorage(*models.BackupStorage) (int, error) {
	return 0, f.err
}
func (f *nameConflictStore) UpdateBackupStorage(*models.BackupStorage) error { return f.err }
func (f *nameConflictStore) GetBackupStorage(id int) (*models.BackupStorage, error) {
	return &models.BackupStorage{ID: id}, nil // a platform row with no secret to merge; not the path under test
}
func (f *nameConflictStore) CreateStorageConnection(*models.StorageConnection) (int, error) {
	return 0, f.err
}
func (f *nameConflictStore) UpdateStorageConnection(*models.StorageConnection) error { return f.err }
func (f *nameConflictStore) GetStorageConnection(int) (*models.StorageConnection, error) {
	return &models.StorageConnection{}, nil // no stored secret to rebind; not the path under test
}

func backupStorageBody() []byte {
	b, _ := json.Marshal(models.BackupStorage{
		Name: "taken", Provider: "node-local", Config: []byte(`{"basePath":"/data"}`),
	})
	return b
}

func connectionBody() []byte {
	b, _ := json.Marshal(storageConnectionRequest{Name: "taken", Provider: "s3", AccessKey: "AKIA"})
	return b
}

func TestStorageHandlersMapNameConflictsAndMissingRows(t *testing.T) {
	cases := []struct {
		name     string
		storeErr error
		call     func(h *AppState, rw http.ResponseWriter, req *http.Request)
		method   string
		path     string
		body     []byte
		wantCode int
	}{
		{
			name:     "backup storage create, duplicate name",
			storeErr: store.ErrNameTaken,
			call:     func(s *AppState, rw http.ResponseWriter, r *http.Request) { NewBackupHandler(s).CreateStorage(rw, r) },
			method:   http.MethodPost, path: "/api/backup-storages", body: backupStorageBody(),
			wantCode: http.StatusConflict,
		},
		{
			name:     "backup storage rename, duplicate name",
			storeErr: store.ErrNameTaken,
			call:     func(s *AppState, rw http.ResponseWriter, r *http.Request) { NewBackupHandler(s).UpdateStorage(rw, r) },
			method:   http.MethodPatch, path: "/api/backup-storages/5", body: backupStorageBody(),
			wantCode: http.StatusConflict,
		},
		{
			name:     "backup storage patch, ghost id",
			storeErr: sql.ErrNoRows,
			call:     func(s *AppState, rw http.ResponseWriter, r *http.Request) { NewBackupHandler(s).UpdateStorage(rw, r) },
			method:   http.MethodPatch, path: "/api/backup-storages/9999", body: backupStorageBody(),
			wantCode: http.StatusNotFound,
		},
		{
			name:     "storage connection create, duplicate name",
			storeErr: store.ErrNameTaken,
			call: func(s *AppState, rw http.ResponseWriter, r *http.Request) {
				NewStorageConnectionsHandler(s).CreateConnection(rw, r)
			},
			method: http.MethodPost, path: "/api/storage-connections", body: connectionBody(),
			wantCode: http.StatusConflict,
		},
		{
			name:     "storage connection rename, duplicate name",
			storeErr: store.ErrNameTaken,
			call: func(s *AppState, rw http.ResponseWriter, r *http.Request) {
				NewStorageConnectionsHandler(s).UpdateConnection(rw, r)
			},
			method: http.MethodPatch, path: "/api/storage-connections/5", body: connectionBody(),
			wantCode: http.StatusConflict,
		},
		{
			name:     "storage connection patch, ghost id",
			storeErr: sql.ErrNoRows,
			call: func(s *AppState, rw http.ResponseWriter, r *http.Request) {
				NewStorageConnectionsHandler(s).UpdateConnection(rw, r)
			},
			method: http.MethodPatch, path: "/api/storage-connections/9999", body: connectionBody(),
			wantCode: http.StatusNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &AppState{Store: &nameConflictStore{err: tc.storeErr}}
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
			req = mux.SetURLVars(req, map[string]string{"id": "5"})
			rw := httptest.NewRecorder()

			tc.call(state, rw, req)

			if rw.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, tc.wantCode, rw.Body.String())
			}
			// The driver's text names the table and the constraint; it must not
			// reach the client on any of these paths.
			if body := rw.Body.String(); strings.Contains(body, "pq:") || strings.Contains(body, "23505") {
				t.Errorf("the raw driver error was echoed to the client: %s", body)
			}
		})
	}
}
