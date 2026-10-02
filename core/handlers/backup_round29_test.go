package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
)

// The restore took the sub-server from the job as it is today. A job pointed
// elsewhere after it ran restored a run of one sub-server as the whole
// container - the node replaced the server root with it.
func TestARestoreGoesWhereTheArchiveWasTaken(t *testing.T) {
	h, _, rdb, _ := presignedOnlyHandler(t, currentNodeVersion, models.BackupRun{
		ID: 1, JobID: 10, Status: "success", StorageKey: "backups/k.tar.gz",
		Manifest: `{"schema":1,"scope":"lobby"}`,
	})
	rw := httptest.NewRecorder()
	h.RestoreRun(rw, backupJobRequest(http.MethodPost, "/api/backup-runs/1/restore", "", map[string]string{"runId": "1"}))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rw.Code, rw.Body.String())
	}
	cmds := queuedCommands(t, rdb)
	if len(cmds) != 1 {
		t.Fatalf("commands = %d, want 1", len(cmds))
	}
	var cmd map[string]interface{}
	if err := json.Unmarshal([]byte(cmds[0]), &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd["subServer"] != "lobby" {
		t.Fatalf("subServer = %v, want the archive's own scope, lobby", cmd["subServer"])
	}
}

// What a job covers cannot move under the runs it already has.
func TestAJobsScopeCannotMoveUnderItsRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs []models.BackupRun
		want int
	}{
		{"with runs", []models.BackupRun{{ID: 1}}, http.StatusConflict},
		{"without runs", nil, http.StatusOK},
	} {
		fs := &backupSubServerFakeStore{runs: tc.runs}
		h := &BackupHandler{state: &AppState{Store: fs, Authz: authz.NewResolver(fs)}}
		rw := httptest.NewRecorder()
		h.UpdateJob(rw, backupJobRequest(http.MethodPatch, "/api/backup-jobs/1", `{"subServer":"lobby"}`, map[string]string{"jobId": "1"}))
		if rw.Code != tc.want {
			t.Errorf("%s: status %d, want %d (body %s)", tc.name, rw.Code, tc.want, rw.Body.String())
		}
		if (tc.want == http.StatusConflict) != (fs.updated == nil) {
			t.Errorf("%s: updated = %+v", tc.name, fs.updated)
		}
	}
}

// Deleting a backup a restore was still waiting on took the archive away
// after the node had already stopped the server for it.
func TestABackupBeingRestoredIsNotDeleted(t *testing.T) {
	h, fs, _, rec := presignedOnlyHandler(t, currentNodeVersion, models.BackupRun{ID: 1, JobID: 10, Status: "success", StorageKey: "backups/k.tar.gz"})
	fs.restoring = true
	rw := httptest.NewRecorder()
	h.DeleteRun(rw, backupJobRequest(http.MethodDelete, "/api/backup-runs/1", "", map[string]string{"runId": "1"}))
	if rw.Code != http.StatusConflict || len(fs.deleted) != 0 || len(rec.requests()) != 0 {
		t.Fatalf("status %d, rows deleted %v, storage calls %d; want 409 and nothing touched", rw.Code, fs.deleted, len(rec.requests()))
	}
}
