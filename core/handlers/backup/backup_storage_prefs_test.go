package backup

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
)

func prefsFixture() *ownStorageFakeStore {
	me, other := "u1", "u2"
	return &ownStorageFakeStore{byID: map[int]*models.BackupStorage{
		5: {ID: 5, Name: "mine", Provider: "s3", OwnerID: &me},
		6: {ID: 6, Name: "mine too", Provider: "s3", OwnerID: &me, IsDefault: true},
		7: {ID: 7, Name: "theirs", Provider: "s3", OwnerID: &other},
		8: {ID: 8, Name: "platform", Provider: "s3"},
		9: {ID: 9, Name: "odd", Provider: "local", OwnerID: &me},
	}, prefBackups: intPtr(6)}
}

func putPrefs(h *BackupHandler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.PutStoragePrefs(rec, asUser(httptest.NewRequest(http.MethodPut, "/api/me/storage-prefs", strings.NewReader(body)), "u1"))
	return rec
}

// Only the caller's own s3 storage can be chosen. Foreign, platform and missing
// ids answer the same 404 as the own-storage routes, so the answer does not
// confirm that an id exists; and nothing is written when any field is refused.
func TestPutStoragePrefsRefusesWhatIsNotTheCallers(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"foreign backups", `{"backupsStorageId":7}`, http.StatusNotFound},
		{"platform templates", `{"templatesStorageId":8}`, http.StatusNotFound},
		{"missing", `{"backupsStorageId":404}`, http.StatusNotFound},
		{"not s3", `{"templatesStorageId":9}`, http.StatusBadRequest},
		{"one good one foreign", `{"backupsStorageId":5,"templatesStorageId":7}`, http.StatusNotFound},
		{"not a number", `{"backupsStorageId":"5"}`, http.StatusBadRequest},
		{"nothing to change", `{}`, http.StatusBadRequest},
		{"bad json", `{`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := prefsFixture()
			rec := putPrefs(&BackupHandler{state: withCoreStorage(&handlers.AppState{Store: st})}, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if st.backupsWrites+st.templatesWrites != 0 {
				t.Fatalf("refused request still wrote %d/%d", st.backupsWrites, st.templatesWrites)
			}
		})
	}
}

// Each column is written on its own: a field left out is not touched, so the
// two selectors on the Storage page cannot undo each other. Choosing Dylaris
// storage (null) for backups also clears the default flag that a NULL pref
// falls back to.
func TestPutStoragePrefsWritesEachColumnIndependently(t *testing.T) {
	tests := []struct {
		name                       string
		body                       string
		wantBackups, wantTemplates *int
		wantBW, wantTW             int
		wantDefault                int // id of the own storage flagged default, 0 = none
	}{
		{"templates only", `{"templatesStorageId":5}`, intPtr(6), intPtr(5), 0, 1, 6},
		{"backups to own", `{"backupsStorageId":5}`, intPtr(5), nil, 1, 0, 5},
		{"backups to dylaris clears default", `{"backupsStorageId":null}`, nil, nil, 1, 0, 0},
		{"both", `{"backupsStorageId":null,"templatesStorageId":6}`, nil, intPtr(6), 1, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := prefsFixture()
			rec := putPrefs(&BackupHandler{state: withCoreStorage(&handlers.AppState{Store: st})}, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
			}
			if st.backupsWrites != tc.wantBW || st.templatesWrites != tc.wantTW {
				t.Errorf("writes = %d/%d, want %d/%d", st.backupsWrites, st.templatesWrites, tc.wantBW, tc.wantTW)
			}
			if !sameInt(st.prefBackups, tc.wantBackups) || !sameInt(st.prefTemplates, tc.wantTemplates) {
				t.Errorf("prefs = %v/%v, want %v/%v", st.prefBackups, st.prefTemplates, tc.wantBackups, tc.wantTemplates)
			}
			def := 0
			if bs, _ := st.GetUserDefaultBackupStorage("u1"); bs != nil {
				def = bs.ID
			}
			if def != tc.wantDefault {
				t.Errorf("default = %d, want %d", def, tc.wantDefault)
			}
		})
	}
}

func TestPutStoragePrefsBodyIsCapped(t *testing.T) {
	st := prefsFixture()
	rec := putPrefs(&BackupHandler{state: withCoreStorage(&handlers.AppState{Store: st})}, `{"backupsStorageId":5,"pad":"`+strings.Repeat("x", 8<<10)+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge || st.backupsWrites != 0 {
		t.Fatalf("code = %d, %d writes; want 413 and none", rec.Code, st.backupsWrites)
	}
}

func TestPutStoragePrefsFailureIsGeneric(t *testing.T) {
	st := prefsFixture()
	st.failPrefWrite = true
	rec := putPrefs(&BackupHandler{state: withCoreStorage(&handlers.AppState{Store: st})}, `{"backupsStorageId":5}`)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("code = %d body = %s; want a generic 500", rec.Code, rec.Body.String())
	}
}

// GET reports what is actually used: a NULL backups pref with an own default
// still set resolves to that default, so it is shown as chosen.
func TestGetStoragePrefs(t *testing.T) {
	tests := []struct {
		name        string
		pref        *int
		wantBackups *int
	}{
		{"explicit pref", intPtr(5), intPtr(5)},
		{"null pref falls back to the own default", nil, intPtr(6)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := prefsFixture()
			st.prefBackups, st.prefTemplates = tc.pref, intPtr(5)
			rec := httptest.NewRecorder()
			(&BackupHandler{state: withCoreStorage(&handlers.AppState{Store: st})}).GetStoragePrefs(rec,
				asUser(httptest.NewRequest(http.MethodGet, "/api/me/storage-prefs", bytes.NewReader(nil)), "u1"))
			var got struct {
				Success            bool `json:"success"`
				BackupsStorageID   *int `json:"backupsStorageId"`
				TemplatesStorageID *int `json:"templatesStorageId"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil || !got.Success {
				t.Fatalf("decode: %v %+v", err, got)
			}
			if !sameInt(got.BackupsStorageID, tc.wantBackups) || !sameInt(got.TemplatesStorageID, intPtr(5)) {
				t.Errorf("got %v/%v, want %v/5", got.BackupsStorageID, got.TemplatesStorageID, tc.wantBackups)
			}
		})
	}
}
