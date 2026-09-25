package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// backupHonestyStore answers the calls these three paths make. Anything else
// panics on the nil embedded store.Store, which the harness turns into a 500.
type backupHonestyStore struct {
	store.Store
	run       *models.BackupRun
	job       *models.BackupJob
	storages  map[int]*models.BackupStorage
	createErr error
	created   *models.BackupJob
}

func (f *backupHonestyStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, OwnerID: "owner-1"}, nil
}
func (f *backupHonestyStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}
func (f *backupHonestyStore) GetServerGrant(int, string) (*store.ServerGrant, error) {
	return nil, nil
}
func (f *backupHonestyStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	return nil, nil
}
func (f *backupHonestyStore) GetBackupRun(int) (*models.BackupRun, error) { return f.run, nil }
func (f *backupHonestyStore) GetBackupJob(int) (*models.BackupJob, error) { return f.job, nil }
func (f *backupHonestyStore) GetBackupStorage(id int) (*models.BackupStorage, error) {
	return f.storages[id], nil
}
func (f *backupHonestyStore) GetDefaultBackupStorage() (*models.BackupStorage, error) {
	return nil, nil
}
func (f *backupHonestyStore) GetUserDefaultBackupStorage(string) (*models.BackupStorage, error) {
	return nil, nil
}
func (f *backupHonestyStore) CreateBackupJob(j *models.BackupJob) (int, error) {
	f.created = j
	return 1, f.createErr
}

func backupReq(method, path, body, userID string, vars map[string]string) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	r.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(r.Context(), "userID", userID)
	ctx = context.WithValue(ctx, "isAdmin", false)
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

func backupHandlerFor(st store.Store) *BackupHandler {
	return &BackupHandler{state: &AppState{Store: st, Authz: authz.NewResolver(st)}}
}

// A stranger asked to restore a run and was told "cannot restore a run that did
// not complete successfully" - a fact about somebody else's backup. The status
// check ran before the access check, so run ids could be walked to learn which
// exist and which succeeded. Authorization comes first now.
func TestRestoreAsksWhoBeforeItAnswersWhat(t *testing.T) {
	st := &backupHonestyStore{
		run: &models.BackupRun{ID: 1, JobID: 1, Status: "failed"},
		job: &models.BackupJob{ID: 1, ServerID: 7},
	}
	h := backupHandlerFor(st)

	rec := httptest.NewRecorder()
	h.RestoreRun(rec, backupReq(http.MethodPost, "/api/backup-runs/1/restore", "{}", "stranger", map[string]string{"runId": "1"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a stranger must not learn a run's state", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "complete successfully") {
		t.Errorf("the refusal described the run: %s", rec.Body.String())
	}

	// The owner still gets the real reason, which is the point of keeping it.
	rec = httptest.NewRecorder()
	h.RestoreRun(rec, backupReq(http.MethodPost, "/api/backup-runs/1/restore", "{}", "owner-1", map[string]string{"runId": "1"}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "complete successfully") {
		t.Fatalf("owner got %d %s, want 400 with the real reason", rec.Code, rec.Body.String())
	}
}

// A job naming a storage that cannot be used was answered with the database's
// own foreign-key error, verbatim, with a 500 - constraint name and SQLSTATE
// included. A job naming ANOTHER TENANT'S storage was worse: accepted, listed,
// enabled, and failing every time it ran.
func TestABackupJobCannotNameAStorageItMayNotUse(t *testing.T) {
	other := "somebody-else"
	st := &backupHonestyStore{storages: map[int]*models.BackupStorage{
		9: {ID: 9, OwnerID: &other},
	}}
	h := backupHandlerFor(st)

	for _, tc := range []struct {
		name, body, want string
	}{
		{"a storage that does not exist", `{"name":"x","schedule":"manual","storageId":404}`, "No such backup storage"},
		{"another account's storage", `{"name":"x","schedule":"manual","storageId":9}`, "belongs to another account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.CreateJob(rec, backupReq(http.MethodPost, "/api/servers/7/backup-jobs", tc.body, "owner-1", map[string]string{"id": "7"}))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body = %s, want it to mention %q", rec.Body.String(), tc.want)
			}
			for _, leak := range []string{"pq:", "constraint", "SQLSTATE", "23503"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("the answer carried database internals (%q): %s", leak, rec.Body.String())
				}
			}
			if st.created != nil {
				t.Error("the job was written despite the refusal")
			}
		})
	}

	t.Run("naming no storage is still fine", func(t *testing.T) {
		st.created = nil
		rec := httptest.NewRecorder()
		h.CreateJob(rec, backupReq(http.MethodPost, "/api/servers/7/backup-jobs", `{"name":"x","schedule":"manual"}`, "owner-1", map[string]string{"id": "7"}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: the resolver picks a default at run time: %s", rec.Code, rec.Body.String())
		}
	})
}
