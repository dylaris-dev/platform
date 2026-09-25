package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// platformBackupUnsetStore is an installation that has never set a platform
// backup passphrase: the settings row simply is not there, which the real store
// reports as sql.ErrNoRows.
type platformBackupUnsetStore struct {
	store.Store
	value  string
	hasRow bool
	saved  string
	job    *models.PlatformBackupJob
}

func (f *platformBackupUnsetStore) GetSetting(key string) (string, error) {
	if !f.hasRow {
		return "", sql.ErrNoRows
	}
	return f.value, nil
}
func (f *platformBackupUnsetStore) SetSetting(key, value string) error {
	f.saved = value
	f.hasRow = true
	f.value = value
	return nil
}
func (f *platformBackupUnsetStore) GetPlatformBackupJob(id int) (*models.PlatformBackupJob, error) {
	return f.job, nil
}

func pbReq(method, body string, vars map[string]string) *http.Request {
	r := httptest.NewRequest(method, "/api/platform-backups/passphrase", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(r.Context(), "userID", "admin-1")
	ctx = context.WithValue(ctx, "isAdmin", true)
	if vars != nil {
		return mux.SetURLVars(r.WithContext(ctx), vars)
	}
	return r.WithContext(ctx)
}

// A platform backup is encrypted with a passphrase the operator sets. On every
// installation that has not set one - which is every installation, at the
// start - the settings row does not exist, and GetSetting reports that as
// sql.ErrNoRows.
//
// Measured on production: the endpoint that reports whether a passphrase is set
// answered 500 "Database error", and the endpoint that SETS the first one
// answered 500 before it could store anything. The feature could not be
// started, and it explained itself as a broken database.
func TestThePassphraseCanBeSetOnAnInstallationThatHasNone(t *testing.T) {
	t.Run("status says not set, rather than failing", func(t *testing.T) {
		h := &PlatformBackupHandler{state: &AppState{Store: &platformBackupUnsetStore{}}}
		rec := httptest.NewRecorder()
		h.PassphraseStatus(rec, pbReq(http.MethodGet, "", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Success bool `json:"success"`
			IsSet   bool `json:"isSet"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body: %v", err)
		}
		if !body.Success || body.IsSet {
			t.Fatalf("success=%v isSet=%v, want success with isSet false", body.Success, body.IsSet)
		}
	})

	t.Run("the first passphrase can be stored", func(t *testing.T) {
		st := &platformBackupUnsetStore{}
		h := &PlatformBackupHandler{state: &AppState{Store: st}}
		rec := httptest.NewRecorder()
		h.SetPassphrase(rec, pbReq(http.MethodPut, `{"passphrase":"a-long-enough-passphrase"}`, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if st.saved == "" {
			t.Error("nothing was stored")
		}
	})

	t.Run("a real database fault is still a fault", func(t *testing.T) {
		// settingOrUnset only forgives the missing row. Anything else has to
		// keep reaching the caller, or an outage reads as "not configured".
		v, err := settingOrUnset(&settingFaultStore{}, "any.key")
		if err == nil {
			t.Fatalf("a broken read returned %q with no error", v)
		}
	})
}

type settingFaultStore struct{ store.Store }

func (f *settingFaultStore) GetSetting(string) (string, error) {
	return "", sql.ErrConnDone
}

// A run that fails answered 200 with success:false, because the body was
// written before WriteHeader and writing a body sends 200 by itself. The block
// even distinguishes a refusal by policy from a broken queue - and neither
// status ever reached the wire.
func TestAFailedPlatformBackupRunAnswersWithAStatus(t *testing.T) {
	st := &platformBackupUnsetStore{job: &models.PlatformBackupJob{
		ID:        1,
		Name:      "nightly",
		Selection: models.PlatformBackupSelection{Database: true},
	}}
	// PlatformDB only has to be NAMED for the handler to build a runner; the
	// run refuses on the missing passphrase long before it dials anything.
	h := &PlatformBackupHandler{state: &AppState{
		Store:      st,
		PlatformDB: services.PGConn{Name: "dylaris_db"},
	}}

	rec := httptest.NewRecorder()
	h.RunJob(rec, pbReq(http.MethodPost, "{}", map[string]string{"id": "1"}))

	if rec.Code == http.StatusOK {
		t.Fatalf("a failed run answered 200: %s", rec.Body.String())
	}
	// No passphrase is a refusal by policy, which the handler separates from a
	// broken queue on purpose.
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a refusal by policy: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), services.ErrNoBackupPassphrase.Error()) {
		t.Errorf("the operator was not told what to do: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "no rows in result set") {
		t.Errorf("the answer blamed the database: %s", rec.Body.String())
	}
}
