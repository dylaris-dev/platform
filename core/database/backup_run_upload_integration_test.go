package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// Document F: backup_runs.upload_id and part_size. The conditional write is the
// whole idempotency of starting a multipart upload - two Core replicas answering
// the same node request each create an upload, and exactly one may be stored -
// so it is proven against Postgres, not a fake. The NULL columns are also the
// shape every existing run has, and every run query scans them.
func TestIntegrationBackupRunUploadIsStoredOnce(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	jobID, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: "f3", Schedule: "manual", RetentionCount: 3, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}
	t.Cleanup(func() { st.DeleteBackupJob(jobID) })

	runID, err := st.CreateBackupRun(&models.BackupRun{JobID: jobID, Status: "running", StorageKey: "backups/f3/run.tar.gz"})
	if err != nil {
		t.Fatalf("CreateBackupRun: %v", err)
	}

	// A run from before the columns: NULL reads as no upload.
	run, err := st.GetBackupRun(runID)
	if err != nil {
		t.Fatalf("GetBackupRun on a run with NULL upload columns: %v", err)
	}
	if run.UploadID != "" || run.PartSize != 0 {
		t.Fatalf("new run has upload %q part size %d, want none", run.UploadID, run.PartSize)
	}

	const partSize = 64 << 20
	stored, err := st.SetBackupRunUpload(runID, "upload-first", partSize)
	if err != nil || !stored {
		t.Fatalf("first SetBackupRunUpload = %v, %v; want stored", stored, err)
	}
	stored, err = st.SetBackupRunUpload(runID, "upload-second", 5<<20)
	if err != nil || stored {
		t.Fatalf("second SetBackupRunUpload = %v, %v; want refused", stored, err)
	}
	run, err = st.GetBackupRun(runID)
	if err != nil {
		t.Fatalf("GetBackupRun: %v", err)
	}
	if run.UploadID != "upload-first" || run.PartSize != partSize {
		t.Fatalf("stored upload %q part size %d, want upload-first / %d", run.UploadID, run.PartSize, partSize)
	}

	// Every other reader of the column list still scans.
	if runs, err := st.ListBackupRuns(jobID, 10); err != nil || len(runs) != 1 || runs[0].UploadID != "upload-first" {
		t.Fatalf("ListBackupRuns = %+v, %v", runs, err)
	}
	if runs, err := st.ListAbandonedBackupRuns(time.Now().Add(time.Hour), 10); err != nil {
		t.Fatalf("ListAbandonedBackupRuns: %v", err)
	} else {
		found := false
		for _, r := range runs {
			if r.ID == runID && r.UploadID == "upload-first" {
				found = true
			}
		}
		if !found {
			t.Fatalf("ListAbandonedBackupRuns did not return run %d with its upload", runID)
		}
	}

	// A run that is no longer running takes no upload, even with none stored.
	closedID, err := st.CreateBackupRun(&models.BackupRun{JobID: jobID, Status: "running", StorageKey: "backups/f3/closed.tar.gz"})
	if err != nil {
		t.Fatalf("CreateBackupRun: %v", err)
	}
	if err := st.UpdateBackupRunStatus(closedID, "failed", "reaped", 0, "backups/f3/closed.tar.gz", time.Now()); err != nil {
		t.Fatalf("UpdateBackupRunStatus: %v", err)
	}
	if stored, err := st.SetBackupRunUpload(closedID, "upload-late", partSize); err != nil || stored {
		t.Fatalf("SetBackupRunUpload on a failed run = %v, %v; want refused", stored, err)
	}
}

// A fresh install gets both columns from the first boot.
func TestIntegrationBackupRunUploadColumnsOnAFreshSchema(t *testing.T) {
	db := freshSchemaDB(t)
	for _, col := range []string{"upload_id", "part_size"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'backup_runs' AND column_name = $1`, col).Scan(&n); err != nil {
			t.Fatalf("query %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("backup_runs.%s missing after one boot", col)
		}
	}
}
