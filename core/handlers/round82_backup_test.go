package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
)

// No restore while the server moves: it restarted the server on the node it
// was leaving. (Two at once are refused by the node, which knows.)
func TestARestoreWaitsForAMove(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		want   int
	}{
		{"moving", "migrating", http.StatusConflict},
		{"free", "stopped", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fs, rdb, _ := presignedOnlyHandler(t, currentNodeVersion, models.BackupRun{ID: 1, JobID: 10, Status: "success", StorageKey: "backups/k.tar.gz"})
			fs.serverStatus = tc.status
			rw := httptest.NewRecorder()
			h.RestoreRun(rw, backupJobRequest(http.MethodPost, "/api/backup-runs/1/restore", "", map[string]string{"runId": "1"}))
			if rw.Code != tc.want {
				t.Fatalf("status %d, want %d (%s)", rw.Code, tc.want, rw.Body)
			}
			if sent := len(queuedCommands(t, rdb)); (tc.want == http.StatusOK) != (sent == 1) {
				t.Fatalf("%d commands sent", sent)
			}
		})
	}
}

// Deleting the schedule purged every archive, including the one a restore had
// already stopped the server for.
func TestAScheduleBeingRestoredIsNotDeleted(t *testing.T) {
	h, fs, _, rec := presignedOnlyHandler(t, currentNodeVersion, models.BackupRun{ID: 1, JobID: 10, Status: "success", StorageKey: "backups/k.tar.gz"})
	fs.restoring = true
	rw := httptest.NewRecorder()
	h.DeleteJob(rw, backupJobRequest(http.MethodDelete, "/api/backup-jobs/10", "", map[string]string{"jobId": "10"}))
	if rw.Code != http.StatusConflict || len(rec.requests()) != 0 {
		t.Fatalf("status %d, storage calls %d; want 409 and nothing touched", rw.Code, len(rec.requests()))
	}
}
