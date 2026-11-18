package backup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

func (f *presignedOnlyStore) ListBackupRuns(int, int) ([]models.BackupRun, error) {
	return []models.BackupRun{f.run}, nil
}
func (f *presignedOnlyStore) ListBackupRunRefsForJob(int) ([]store.BackupRunRef, error) {
	return nil, nil
}
func (f *presignedOnlyStore) DeleteBackupJob(int) error { return nil }

// W08: a run the node is still writing to its own disk was deleted from under
// it. The node removed nothing (the archive existed only as a .partial), Core
// dropped the row, and the finished archive stayed on disk for good, counted
// against the backup quota with nothing able to name it. A run on object
// storage is still deleted: Core aborts its upload.
func TestARunTheNodeIsStillWritingIsNotDeleted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		status   string
		want     int
	}{
		{"node-local running", "node-local", "running", http.StatusConflict},
		{"local running", "local", "running", http.StatusConflict},
		{"s3 running", "s3", "running", http.StatusOK},
	} {
		for _, op := range []string{"run", "schedule"} {
			t.Run(tc.name+" "+op, func(t *testing.T) {
				h, fs, _, _ := presignedOnlyHandler(t, currentNodeVersion, models.BackupRun{ID: 1, JobID: 10, Status: tc.status, StorageKey: "backups/k.tar.gz"})
				if tc.provider != "s3" {
					fs.storage = &models.BackupStorage{ID: 3, Name: "disk", Provider: tc.provider, Config: json.RawMessage(`{"path":"/nonexistent-w08"}`)}
				}
				rw := httptest.NewRecorder()
				if op == "run" {
					h.DeleteRun(rw, backupJobRequest(http.MethodDelete, "/api/backup-runs/1", "", map[string]string{"runId": "1"}))
				} else {
					h.DeleteJob(rw, backupJobRequest(http.MethodDelete, "/api/backup-jobs/10", "", map[string]string{"jobId": "10"}))
				}
				if rw.Code != tc.want {
					t.Fatalf("status %d, want %d (%s)", rw.Code, tc.want, rw.Body)
				}
				if tc.want == http.StatusConflict && len(fs.deleted) != 0 {
					t.Fatalf("rows deleted %v on a refusal", fs.deleted)
				}
			})
		}
	}
}
