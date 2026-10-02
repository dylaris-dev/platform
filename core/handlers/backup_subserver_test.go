package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// backupSubServerFakeStore records whether the job ever reached the store, so a
// test can prove the request was refused BEFORE it was persisted.
type backupSubServerFakeStore struct {
	store.Store
	created *models.BackupJob
	updated *models.BackupJob
	runs    []models.BackupRun
}

func (f *backupSubServerFakeStore) ListBackupRuns(int, int) ([]models.BackupRun, error) {
	return f.runs, nil
}

func (f *backupSubServerFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-uuid", OwnerID: "alice"}, nil
}

func (f *backupSubServerFakeStore) CreateBackupJob(j *models.BackupJob) (int, error) {
	f.created = j
	return 1, nil
}

func (f *backupSubServerFakeStore) GetBackupJob(id int) (*models.BackupJob, error) {
	return &models.BackupJob{ID: id, ServerID: 7, Schedule: "manual", RetentionCount: 3}, nil
}

func (f *backupSubServerFakeStore) UpdateBackupJob(j *models.BackupJob) error {
	f.updated = j
	return nil
}

func backupJobRequest(method, target, body string, vars map[string]string) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	ctx := context.WithValue(req.Context(), "userID", "alice")
	ctx = context.WithValue(ctx, "username", "alice")
	ctx = context.WithValue(ctx, "isAdmin", true) // so the access check is not what refuses
	return mux.SetURLVars(req.WithContext(ctx), vars)
}

// traversalSubServers are the shapes that escape a filepath.Join. The last two
// are not traversals but are still refused: the allowed charset is a single
// directory NAME, so anything carrying a separator cannot be one.
var traversalSubServers = []string{
	"..",
	"../..",
	"../other-tenant",
	"world/../../..",
	"/etc",
	`C:\windows`,
	"a/b",
}

// TestCreateJob_RefusesATraversalSubServer is the finding. models.BackupJob is
// decoded straight from the request body and only ID/ServerID/schedule are
// forced, so SubServer was caller-controlled all the way to the Node, where it
// is filepath.Join'ed onto the server's data directory. Join cleans "..", it
// does not confine - so the backup would have archived whatever the node
// process can read and uploaded it to storage the caller can download from.
func TestCreateJob_RefusesATraversalSubServer(t *testing.T) {
	for _, sub := range traversalSubServers {
		t.Run(sub, func(t *testing.T) {
			fs := &backupSubServerFakeStore{}
			h := &BackupHandler{state: &AppState{Store: fs, Authz: authz.NewResolver(fs)}}
			rw := httptest.NewRecorder()
			body := `{"name":"nightly","schedule":"manual","subServer":` + quote(sub) + `}`
			h.CreateJob(rw, backupJobRequest(http.MethodPost, "/api/servers/7/backup-jobs", body, map[string]string{"id": "7"}))

			if rw.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for subServer %q (body: %s)", rw.Code, sub, rw.Body.String())
			}
			if fs.created != nil {
				t.Errorf("the job was persisted anyway: %+v", fs.created)
			}
		})
	}
}

// TestUpdateJob_RefusesATraversalSubServer covers the other write path: a job
// created with a legitimate name could simply be patched to a traversal one.
func TestUpdateJob_RefusesATraversalSubServer(t *testing.T) {
	fs := &backupSubServerFakeStore{}
	h := &BackupHandler{state: &AppState{Store: fs, Authz: authz.NewResolver(fs)}}
	rw := httptest.NewRecorder()
	body := `{"name":"nightly","schedule":"manual","subServer":"../../.."}`
	h.UpdateJob(rw, backupJobRequest(http.MethodPatch, "/api/backup-jobs/1", body, map[string]string{"jobId": "1"}))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rw.Code, rw.Body.String())
	}
	if fs.updated != nil {
		t.Errorf("the job was updated anyway: %+v", fs.updated)
	}
}

// TestCreateJob_AcceptsAPlainSubServerName is the control: the ordinary case
// must still work, or every multi-world backup job breaks.
func TestCreateJob_AcceptsAPlainSubServerName(t *testing.T) {
	fs := &backupSubServerFakeStore{}
	h := &BackupHandler{state: &AppState{Store: fs, Authz: authz.NewResolver(fs)}}
	rw := httptest.NewRecorder()
	body := `{"name":"nightly","schedule":"manual","subServer":"survival-2"}`
	h.CreateJob(rw, backupJobRequest(http.MethodPost, "/api/servers/7/backup-jobs", body, map[string]string{"id": "7"}))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rw.Code, rw.Body.String())
	}
	if fs.created == nil || fs.created.SubServer == nil || *fs.created.SubServer != "survival-2" {
		t.Errorf("job not persisted with its sub-server: %+v", fs.created)
	}
}

// TestCreateJob_AcceptsAnAbsentSubServer is the second control: NULL / empty
// means "the whole container", which is the default job shape.
func TestCreateJob_AcceptsAnAbsentSubServer(t *testing.T) {
	fs := &backupSubServerFakeStore{}
	h := &BackupHandler{state: &AppState{Store: fs, Authz: authz.NewResolver(fs)}}
	rw := httptest.NewRecorder()
	h.CreateJob(rw, backupJobRequest(http.MethodPost, "/api/servers/7/backup-jobs",
		`{"name":"nightly","schedule":"manual"}`, map[string]string{"id": "7"}))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rw.Code, rw.Body.String())
	}
	if fs.created == nil {
		t.Fatal("job was not persisted")
	}
	if fs.created.SubServer != nil {
		t.Errorf("SubServer = %q, want nil", *fs.created.SubServer)
	}
}

// quote renders s as a JSON string literal, so a backslash in a Windows-style
// payload survives into the request body.
func quote(s string) string {
	out := []byte{'"'}
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' || s[i] == '"' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(append(out, '"'))
}
