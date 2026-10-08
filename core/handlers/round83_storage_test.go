package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// inUseStorageStore refuses every delete the way the store does for a storage
// a backup or a schedule still names.
type inUseStorageStore struct{ ownStorageFakeStore }

func (f *inUseStorageStore) DeleteBackupStorage(int) error { return store.ErrStorageInUse }

// A storage still in use answers 409 on both delete routes, not a 500 carrying
// the store's error text.
func TestAStorageInUseIsAConflict(t *testing.T) {
	me := "u1"
	st := &inUseStorageStore{ownStorageFakeStore{byID: map[int]*models.BackupStorage{
		7: {ID: 7, Name: "mine", Provider: "s3", OwnerID: &me},
		8: {ID: 8, Name: "the platform's", Provider: "s3"},
	}}}
	h := &BackupHandler{state: &AppState{Store: st}}

	rec := httptest.NewRecorder()
	h.DeleteOwnStorage(rec, mux.SetURLVars(asUser(httptest.NewRequest(http.MethodDelete, "/x", nil), me), map[string]string{"id": "7"}))
	if rec.Code != http.StatusConflict {
		t.Errorf("own delete: %d, want 409 (%s)", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.DeleteStorage(rec, mux.SetURLVars(httptest.NewRequest(http.MethodDelete, "/x", nil), map[string]string{"id": "8"}))
	if rec.Code != http.StatusConflict {
		t.Errorf("admin delete: %d, want 409 (%s)", rec.Code, rec.Body)
	}
}

// The admin storage routes are for PLATFORM storages. A tenant's row answered
// them: an update could move it to a provider the tenant guard does not cover,
// or, with isDefault, clear the platform's default.
func TestAdminStorageRoutesRefuseATenantsRow(t *testing.T) {
	tenant := "u2"
	st := &ownStorageFakeStore{byID: map[int]*models.BackupStorage{
		7: {ID: 7, Name: "theirs", Provider: "s3", OwnerID: &tenant},
		8: {ID: 8, Name: "the platform's", Provider: "s3"},
	}}
	h := &BackupHandler{state: &AppState{Store: st}}
	vars := func(r *http.Request, id string) *http.Request { return mux.SetURLVars(r, map[string]string{"id": id}) }

	for _, tc := range []struct {
		name string
		call func(id string) int
	}{
		{"update", func(id string) int {
			rec := httptest.NewRecorder()
			h.UpdateStorage(rec, vars(httptest.NewRequest(http.MethodPatch, "/x", bytes.NewReader(s3Body("renamed", map[string]any{"isDefault": true}))), id))
			return rec.Code
		}},
		{"delete", func(id string) int {
			rec := httptest.NewRecorder()
			h.DeleteStorage(rec, vars(httptest.NewRequest(http.MethodDelete, "/x", nil), id))
			return rec.Code
		}},
		{"test", func(id string) int {
			rec := httptest.NewRecorder()
			h.TestStorage(rec, vars(httptest.NewRequest(http.MethodPost, "/x", nil), id))
			return rec.Code
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := tc.call("7"); code != http.StatusNotFound {
				t.Errorf("tenant row: %d, want 404", code)
			}
		})
	}
	if len(st.deleted) != 0 || len(st.updated) != 0 {
		t.Fatalf("a tenant row was written: deleted=%v updated=%+v", st.deleted, st.updated)
	}

	// The platform row still works, and an update cannot hand it an owner.
	rec := httptest.NewRecorder()
	h.UpdateStorage(rec, vars(httptest.NewRequest(http.MethodPatch, "/x", bytes.NewReader(s3Body("renamed", map[string]any{"ownerId": tenant}))), "8"))
	if rec.Code != http.StatusOK || len(st.updated) != 1 || st.updated[0].OwnerID != nil {
		t.Fatalf("platform update: %d %+v", rec.Code, st.updated)
	}
	rec = httptest.NewRecorder()
	h.DeleteStorage(rec, vars(httptest.NewRequest(http.MethodDelete, "/x", nil), "8"))
	if rec.Code != http.StatusOK || len(st.deleted) != 1 {
		t.Fatalf("platform delete: %d %v", rec.Code, st.deleted)
	}
}

// CreateStorage is the same admin surface: an ownerId in the body filed a new
// storage, of any provider, into a tenant's account.
func TestAdminCreateStorageIsAlwaysAPlatformStorage(t *testing.T) {
	st := &ownStorageFakeStore{byID: map[int]*models.BackupStorage{}}
	h := &BackupHandler{state: &AppState{Store: st}}
	rec := httptest.NewRecorder()
	h.CreateStorage(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(s3Body("new", map[string]any{"ownerId": "u2", "isDefault": true}))))
	if rec.Code != http.StatusOK || len(st.created) != 1 || st.created[0].OwnerID != nil {
		t.Fatalf("create: %d %+v", rec.Code, st.created)
	}
}
