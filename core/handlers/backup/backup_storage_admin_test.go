package backup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type adminStorageFakeStore struct {
	ownStorageFakeStore
	inUse bool
}

func (f *adminStorageFakeStore) BackupStorageUsage(int) (models.BackupStorageUsage, error) {
	return models.BackupStorageUsage{Jobs: 1, Runs: 2, Templates: 3, Prefs: 1}, nil
}

func (f *adminStorageFakeStore) DeleteBackupStorage(id int) error {
	if f.inUse {
		return store.ErrStorageInUse
	}
	return f.ownStorageFakeStore.DeleteBackupStorage(id)
}

func newAdminStorageFake() *adminStorageFakeStore {
	u1, u2 := "u1", "u2"
	return &adminStorageFakeStore{ownStorageFakeStore: ownStorageFakeStore{byID: map[int]*models.BackupStorage{
		1: {ID: 1, Name: "platform", Provider: "s3"},
		2: {ID: 2, Name: "mine", Provider: "s3", OwnerID: &u1,
			Config: json.RawMessage(`{"endpoint":"https://s3.example.com:9000","bucket":"bk","prefix":"p/","accessKeyId":"AKIAEXAMPLEKEY","secretAccessKey":"topsecret"}`)},
		3: {ID: 3, Name: "theirs", Provider: "s3", OwnerID: &u2},
	}}}
}

func TestAdminListUserStoragesOmitsCredentials(t *testing.T) {
	st := newAdminStorageFake()
	h := &BackupHandler{state: &handlers.AppState{Store: st}}
	rec := httptest.NewRecorder()
	req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/x", nil), map[string]string{"id": "u1"})
	h.AdminListUserStorages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list returned %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leak := range []string{"AKIAEXAMPLEKEY", "topsecret", "accessKeyId", "secretAccessKey"} {
		if strings.Contains(body, leak) {
			t.Errorf("response carries %q: %s", leak, body)
		}
	}
	var resp struct {
		Storages []adminStorageTarget `json:"storages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Storages) != 1 {
		t.Fatalf("got %d targets, want only u1's one", len(resp.Storages))
	}
	got := resp.Storages[0]
	if got.EndpointHost != "s3.example.com" || got.Bucket != "bk" || got.Prefix != "p/" || got.Usage.Templates != 3 {
		t.Errorf("target = %+v", got)
	}
}

func TestAdminDeleteUserStorage(t *testing.T) {
	cases := []struct {
		name      string
		user, sid string
		inUse     bool
		want      int
	}{
		{"own target", "u1", "2", false, http.StatusOK},
		{"in use", "u1", "2", true, http.StatusConflict},
		{"another user's target", "u1", "3", false, http.StatusNotFound},
		{"platform storage", "u1", "1", false, http.StatusNotFound},
		{"unknown", "u1", "99", false, http.StatusNotFound},
		{"bad id", "u1", "x", false, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newAdminStorageFake()
			st.inUse = tc.inUse
			h := &BackupHandler{state: &handlers.AppState{Store: st}}
			rec := httptest.NewRecorder()
			req := mux.SetURLVars(httptest.NewRequest(http.MethodDelete, "/x", nil), map[string]string{"id": tc.user, "storageId": tc.sid})
			h.AdminDeleteUserStorage(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if deleted := len(st.deleted) == 1; deleted != (tc.want == http.StatusOK) {
				t.Errorf("deleted = %v", st.deleted)
			}
		})
	}
}
