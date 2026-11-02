package storageapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/services/corestorage"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// connFakeStore records what the handler asked the store to do, so a request's
// effect on persistence can be asserted. Embeds store.Store so only the methods
// exercised here need bodies. Local to this file; never redeclared.
type connFakeStore struct {
	store.Store
	created    int
	updated    int
	secretSets int
	deleted    int
	lastSecret string

	// existing is what UpdateConnection reads back to decide whether this edit
	// would rebind the stored secret. nil means a connection that holds no
	// secret, which is the case with nothing to guard.
	existing    *models.StorageConnection
	existingErr error

	// templatesOn is the template count per connection; settings is the
	// settings table, for templates_storage_connection_id.
	templatesOn map[int]int
	settings    map[string]string
}

func (f *connFakeStore) CountTemplatesOnStorageConnection(id int) (int, error) {
	return f.templatesOn[id], nil
}
func (f *connFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }

func (f *connFakeStore) GetStorageConnection(id int) (*models.StorageConnection, error) {
	if f.existingErr != nil {
		return nil, f.existingErr
	}
	if f.existing == nil {
		return &models.StorageConnection{ID: id}, nil
	}
	return f.existing, nil
}

func (f *connFakeStore) CreateStorageConnection(*models.StorageConnection) (int, error) {
	f.created++
	return 1, nil
}
func (f *connFakeStore) UpdateStorageConnection(*models.StorageConnection) error {
	f.updated++
	return nil
}
func (f *connFakeStore) SetStorageConnectionSecret(_ int, secret string) error {
	f.secretSets++
	f.lastSecret = secret
	return nil
}
func (f *connFakeStore) DeleteStorageConnection(int) error {
	f.deleted++
	return nil
}

func TestCreateConnection_RejectsUnknownProviderBeforePersisting(t *testing.T) {
	fs := &connFakeStore{}
	h := NewStorageConnectionsHandler(testDeps(Deps{Store: fs}))

	body, _ := json.Marshal(storageConnectionRequest{Name: "nas", Provider: "ftp"})
	rw := httptest.NewRecorder()
	h.CreateConnection(rw, httptest.NewRequest(http.MethodPost, "/api/storage-connections", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("CreateConnection status = %d, want 400 (%s)", rw.Code, rw.Body.String())
	}
	if fs.created != 0 {
		t.Errorf("an unsupported provider was persisted (%d creates); it must be rejected at the boundary", fs.created)
	}
}

func TestCreateConnection_RejectsMissingName(t *testing.T) {
	fs := &connFakeStore{}
	h := NewStorageConnectionsHandler(testDeps(Deps{Store: fs}))

	body, _ := json.Marshal(storageConnectionRequest{Provider: "s3"})
	rw := httptest.NewRecorder()
	h.CreateConnection(rw, httptest.NewRequest(http.MethodPost, "/api/storage-connections", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("CreateConnection status = %d, want 400 (%s)", rw.Code, rw.Body.String())
	}
	if fs.created != 0 {
		t.Errorf("a nameless connection was persisted (%d creates)", fs.created)
	}
}

func TestCreateConnection_AcceptsS3(t *testing.T) {
	fs := &connFakeStore{}
	h := NewStorageConnectionsHandler(testDeps(Deps{Store: fs}))

	body, _ := json.Marshal(storageConnectionRequest{Name: "nas", Provider: "s3", AccessKey: "AKIA", SecretAccessKey: "sk"})
	rw := httptest.NewRecorder()
	h.CreateConnection(rw, httptest.NewRequest(http.MethodPost, "/api/storage-connections", bytes.NewReader(body)))

	if rw.Code != http.StatusOK {
		t.Fatalf("CreateConnection status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.created != 1 {
		t.Errorf("a valid s3 connection was not persisted (%d creates)", fs.created)
	}
}

// TestUpdateConnection_BlankSecretDoesNotRotate is the write-only guard: an edit
// that carries no secret (the panel's redacted default) must update metadata but
// leave the stored secret untouched.
func TestUpdateConnection_BlankSecretDoesNotRotate(t *testing.T) {
	fs := &connFakeStore{}
	h := NewStorageConnectionsHandler(testDeps(Deps{Store: fs}))

	body, _ := json.Marshal(storageConnectionRequest{Name: "nas", Provider: "s3", AccessKey: "AKIA"}) // no SecretAccessKey
	req := httptest.NewRequest(http.MethodPatch, "/api/storage-connections/5", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": "5"})
	rw := httptest.NewRecorder()
	h.UpdateConnection(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("UpdateConnection status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.updated != 1 {
		t.Errorf("metadata update did not run (%d updates)", fs.updated)
	}
	if fs.secretSets != 0 {
		t.Errorf("a blank secret rotated the credential (%d secret writes); it must be left intact", fs.secretSets)
	}
}

// TestUpdateConnection_NonBlankSecretRotates: a submitted secret is rotated
// exactly once, with the submitted value.
func TestUpdateConnection_NonBlankSecretRotates(t *testing.T) {
	fs := &connFakeStore{}
	h := NewStorageConnectionsHandler(testDeps(Deps{Store: fs}))

	body, _ := json.Marshal(storageConnectionRequest{Name: "nas", Provider: "s3", AccessKey: "AKIA", SecretAccessKey: "new-secret"})
	req := httptest.NewRequest(http.MethodPatch, "/api/storage-connections/5", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": "5"})
	rw := httptest.NewRecorder()
	h.UpdateConnection(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("UpdateConnection status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.updated != 1 {
		t.Errorf("metadata update did not run (%d updates)", fs.updated)
	}
	if fs.secretSets != 1 || fs.lastSecret != "new-secret" {
		t.Errorf("submitted secret was not rotated exactly once with the right value (sets=%d, last=%q)", fs.secretSets, fs.lastSecret)
	}
}

func TestUpdateConnection_RejectsUnknownProvider(t *testing.T) {
	fs := &connFakeStore{}
	h := NewStorageConnectionsHandler(testDeps(Deps{Store: fs}))

	body, _ := json.Marshal(storageConnectionRequest{Name: "nas", Provider: "ftp"})
	req := httptest.NewRequest(http.MethodPatch, "/api/storage-connections/5", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": "5"})
	rw := httptest.NewRecorder()
	h.UpdateConnection(rw, req)

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("UpdateConnection status = %d, want 400 (%s)", rw.Code, rw.Body.String())
	}
	if fs.updated != 0 || fs.secretSets != 0 {
		t.Errorf("an unsupported provider was persisted on update (updates=%d, secrets=%d)", fs.updated, fs.secretSets)
	}
}

// TestStorageConnectionProvider_RejectsNonS3: the provider builder refuses any
// non-s3 connection rather than silently mishandling it.
func TestStorageConnectionProvider_RejectsNonS3(t *testing.T) {
	_, err := corestorage.ConnectionProvider(&models.StorageConnection{Provider: "webdav"})
	if err == nil {
		t.Fatal("storageConnectionProvider(webdav) err = nil, want an unsupported-provider error")
	}
}

func TestTestConnection_InvalidID(t *testing.T) {
	fs := &connFakeStore{}
	h := NewStorageConnectionsHandler(testDeps(Deps{Store: fs}))

	req := httptest.NewRequest(http.MethodPost, "/api/storage-connections/abc/test", nil)
	req = mux.SetURLVars(req, map[string]string{"id": "abc"})
	rw := httptest.NewRecorder()
	h.TestConnection(rw, req)

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("TestConnection status = %d, want 400 for a non-numeric id", rw.Code)
	}
}

// A connection templates still use is refused; deleting it would leave those
// templates with files nothing can reach or delete.
func TestDeleteConnection_RefusedWhileTemplatesUseIt(t *testing.T) {
	tests := []struct {
		name        string
		templatesOn map[int]int
		setting     string
		want        int
	}{
		{"unused", nil, "", http.StatusOK},
		{"another connection is used", map[int]int{4: 2}, "4", http.StatusOK},
		{"a template is stored on it", map[int]int{3: 1}, "", http.StatusConflict},
		{"new templates go to it", nil, "3", http.StatusConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := &connFakeStore{templatesOn: tc.templatesOn, settings: map[string]string{"templates_storage_connection_id": tc.setting}}
			h := NewStorageConnectionsHandler(testDeps(Deps{Store: fs}))
			rw := httptest.NewRecorder()
			req := mux.SetURLVars(httptest.NewRequest(http.MethodDelete, "/api/storage-connections/3", nil), map[string]string{"id": "3"})
			h.DeleteConnection(rw, req)
			if rw.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, tc.want, rw.Body.String())
			}
			if wantDeleted := tc.want == http.StatusOK; (fs.deleted == 1) != wantDeleted {
				t.Errorf("deleted %d times, want deleted = %v", fs.deleted, wantDeleted)
			}
			if tc.want == http.StatusConflict && !bytes.Contains(rw.Body.Bytes(), []byte("emplate")) {
				t.Errorf("409 body %s does not name templates", rw.Body.String())
			}
		})
	}
}
