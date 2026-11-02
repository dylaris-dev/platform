package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

type switchSettings map[string]string

func (s switchSettings) GetSetting(k string) (string, error) { return s[k], nil }

func ownStorageOffHandler(st *ownStorageFakeStore) *BackupHandler {
	ff := services.NewFeatureFlags(switchSettings{services.OwnStorageSettingKey: "off"})
	return &BackupHandler{state: withCoreStorage(&handlers.AppState{Store: st, FeatureFlags: ff})}
}

// With own storage off, adding an own target, marking one default and choosing
// one answer 403 own_storage_disabled and write nothing; choosing the platform
// storage (null), listing, updating (credential rotation), testing and
// deleting stay open.
func TestOwnStorageSwitchOff(t *testing.T) {
	refused := []struct {
		name string
		call func(h *BackupHandler, rec *httptest.ResponseRecorder)
	}{
		{"create", func(h *BackupHandler, rec *httptest.ResponseRecorder) {
			h.CreateOwnStorage(rec, asUser(httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(s3Body("new", nil))), "u1"))
		}},
		{"update marking it default", func(h *BackupHandler, rec *httptest.ResponseRecorder) {
			h.UpdateOwnStorage(rec, mux.SetURLVars(asUser(httptest.NewRequest(http.MethodPatch, "/x", bytes.NewReader(s3Body("renamed", map[string]any{"isDefault": true}))), "u1"), map[string]string{"id": "5"}))
		}},
		{"choose own for backups", func(h *BackupHandler, rec *httptest.ResponseRecorder) {
			h.PutStoragePrefs(rec, asUser(httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"backupsStorageId":5}`)), "u1"))
		}},
		{"choose own for templates", func(h *BackupHandler, rec *httptest.ResponseRecorder) {
			h.PutStoragePrefs(rec, asUser(httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"templatesStorageId":5}`)), "u1"))
		}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			st := prefsFixture()
			rec := httptest.NewRecorder()
			tc.call(ownStorageOffHandler(st), rec)
			var body struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != http.StatusForbidden || body.Code != "own_storage_disabled" {
				t.Fatalf("got %d %s, want 403 own_storage_disabled", rec.Code, rec.Body.String())
			}
			if len(st.created)+len(st.updated)+st.backupsWrites+st.templatesWrites != 0 {
				t.Fatalf("refused request still wrote: %+v", st)
			}
		})
	}

	t.Run("choose platform storage", func(t *testing.T) {
		st := prefsFixture()
		rec := httptest.NewRecorder()
		ownStorageOffHandler(st).PutStoragePrefs(rec, asUser(httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"backupsStorageId":null}`)), "u1"))
		if rec.Code != http.StatusOK || st.prefBackups != nil {
			t.Fatalf("got %d, pref %v; want 200 and null", rec.Code, st.prefBackups)
		}
	})

	t.Run("update without choosing", func(t *testing.T) {
		st := prefsFixture()
		rec := httptest.NewRecorder()
		ownStorageOffHandler(st).UpdateOwnStorage(rec, mux.SetURLVars(asUser(httptest.NewRequest(http.MethodPatch, "/x", bytes.NewReader(s3Body("renamed", nil))), "u1"), map[string]string{"id": "5"}))
		if rec.Code != http.StatusOK || len(st.updated) != 1 || st.backupsWrites != 0 {
			t.Fatalf("got %d updated=%d prefWrites=%d; want 200, one update, no choice", rec.Code, len(st.updated), st.backupsWrites)
		}
	})

	t.Run("update clearing default", func(t *testing.T) {
		st := prefsFixture()
		rec := httptest.NewRecorder()
		ownStorageOffHandler(st).UpdateOwnStorage(rec, mux.SetURLVars(asUser(httptest.NewRequest(http.MethodPatch, "/x", bytes.NewReader(s3Body("mine too", map[string]any{"isDefault": false}))), "u1"), map[string]string{"id": "6"}))
		if rec.Code != http.StatusOK || st.prefBackups != nil {
			t.Fatalf("got %d pref=%v; want 200 and the backups choice cleared", rec.Code, st.prefBackups)
		}
	})

	t.Run("list", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ownStorageOffHandler(prefsFixture()).ListOwnStorages(rec, asUser(httptest.NewRequest(http.MethodGet, "/x", nil), "u1"))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mine"`) {
			t.Fatalf("got %d %s, want the caller's targets", rec.Code, rec.Body.String())
		}
	})

	// The choices are ignored while off and not shown, so they must not keep
	// the target from being deleted.
	t.Run("delete clears the hidden choices", func(t *testing.T) {
		st := prefsFixture()
		st.prefTemplates = intPtr(6)
		rec := httptest.NewRecorder()
		ownStorageOffHandler(st).DeleteOwnStorage(rec, mux.SetURLVars(asUser(httptest.NewRequest(http.MethodDelete, "/x", nil), "u1"), map[string]string{"id": "6"}))
		if rec.Code != http.StatusOK || len(st.deleted) != 1 || st.prefBackups != nil || st.prefTemplates != nil {
			t.Fatalf("got %d deleted=%v prefs=%v/%v; want 200, deleted, both cleared", rec.Code, st.deleted, st.prefBackups, st.prefTemplates)
		}
	})

	t.Run("get shows platform storage", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ownStorageOffHandler(prefsFixture()).GetStoragePrefs(rec, asUser(httptest.NewRequest(http.MethodGet, "/x", nil), "u1"))
		if !strings.Contains(rec.Body.String(), `"backupsStorageId":null`) {
			t.Fatalf("got %s, want null choices", rec.Body.String())
		}
	})
}

// jobSwitchStore is the prefs fixture plus what a job write and a new run
// resolve through: the server (owned by u1, a non-admin), the users, and the
// platform default (storage 8).
type jobSwitchStore struct {
	*ownStorageFakeStore
}

func (j jobSwitchStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, OwnerID: "u1"}, nil
}

func (j jobSwitchStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, IsAdmin: id == "admin"}, nil
}

func (j jobSwitchStore) GetDefaultBackupStorage() (*models.BackupStorage, error) {
	return j.byID[8], nil
}

func jobSwitchHandler(audience string) *BackupHandler {
	ff := services.NewFeatureFlags(switchSettings{services.OwnStorageSettingKey: audience})
	return &BackupHandler{state: withCoreStorage(&handlers.AppState{Store: jobSwitchStore{prefsFixture()}, FeatureFlags: ff})}
}

// A job may not be pointed at an own target while own storage is off for the
// server's owner; a job that already points there stays editable, and a
// platform storage is always fine. Audience "admin" with an ADMIN actor still
// refuses: the switch is resolved for the owner (u1), not for who edits.
func TestJobStorageWithOwnStorageOff(t *testing.T) {
	tests := []struct {
		name          string
		audience      string
		actorAdmin    bool
		storage, prev *int
		want          int
	}{
		{"create on own, off", "off", false, intPtr(5), nil, http.StatusForbidden},
		{"change to own, off", "off", false, intPtr(5), intPtr(6), http.StatusForbidden},
		{"unchanged own, off", "off", false, intPtr(6), intPtr(6), 0},
		{"create on platform, off", "off", false, intPtr(8), nil, 0},
		{"create on own, all", "all", false, intPtr(5), nil, 0},
		{"admin actor, non-admin owner, admin audience", "admin", true, intPtr(5), nil, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := asUser(httptest.NewRequest(http.MethodPost, "/x", nil), "admin")
			r = r.WithContext(context.WithValue(r.Context(), "isAdmin", tc.actorAdmin)) //nolint:staticcheck // matches the middleware's key
			refused := jobSwitchHandler(tc.audience).refuseUnusableStorage(rec, r, 1, tc.storage, tc.prev)
			if tc.want == 0 {
				if refused {
					t.Fatalf("refused with %d %s", rec.Code, rec.Body.String())
				}
				return
			}
			if !refused || rec.Code != tc.want || !strings.Contains(rec.Body.String(), "own_storage_disabled") {
				t.Fatalf("got refused=%v %d %s, want %d own_storage_disabled", refused, rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

// The manual run path resolves the switch for the server's OWNER. An admin
// pressing run on a non-admin's server under audience "admin" does not lift
// the owner's job onto their own target.
func TestManualRunStorageFollowsTheOwner(t *testing.T) {
	ctx := context.WithValue(context.Background(), "isAdmin", true) //nolint:staticcheck // matches the middleware's key
	for _, tc := range []struct {
		audience string
		wantID   int
	}{{"admin", 8}, {"off", 8}, {"all", 5}} {
		bs, err := jobSwitchHandler(tc.audience).newRunStorage(ctx, intPtr(5), "u1")
		if err != nil || bs == nil || bs.ID != tc.wantID {
			t.Errorf("%s: got %+v, %v; want storage %d", tc.audience, bs, err, tc.wantID)
		}
	}
}
