package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// Document F review fixes that live in SQL: the reaper measures a multipart
// run's silence from its latest transfer activity (M2), Core's measured size is
// recorded only on a running run (H1), and a running run's node-reported size
// is not usage (H1).

func TestIntegrationBackupRunTransferActivityKeepsARunAlive(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)

	jobID, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: "m2", Schedule: "manual", RetentionCount: 3, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}
	t.Cleanup(func() { st.DeleteBackupJob(jobID) })

	// All three started eight hours ago, beyond the six-hour window.
	mkOldRun := func(key string) int {
		t.Helper()
		id, err := st.CreateBackupRun(&models.BackupRun{JobID: jobID, Status: "running", StorageKey: key})
		if err != nil {
			t.Fatalf("CreateBackupRun: %v", err)
		}
		if _, err := db.Exec(`UPDATE backup_runs SET started_at = NOW() - INTERVAL '8 hours' WHERE id = $1`, id); err != nil {
			t.Fatalf("age run: %v", err)
		}
		return id
	}
	uploading := mkOldRun("backups/m2/uploading.tar.gz")
	stale := mkOldRun("backups/m2/stale.tar.gz")
	silent := mkOldRun("backups/m2/silent.tar.gz")

	if err := st.TouchBackupRunTransfer(uploading); err != nil {
		t.Fatalf("TouchBackupRunTransfer: %v", err)
	}
	if _, err := db.Exec(`UPDATE backup_runs SET transfer_activity_at = NOW() - INTERVAL '7 hours' WHERE id = $1`, stale); err != nil {
		t.Fatalf("age activity: %v", err)
	}

	runs, err := st.ListAbandonedBackupRuns(time.Now().Add(-6*time.Hour), 1000)
	if err != nil {
		t.Fatalf("ListAbandonedBackupRuns: %v", err)
	}
	listed := map[int]bool{}
	for _, r := range runs {
		listed[r.ID] = true
	}
	if listed[uploading] {
		t.Error("a run that asked for part URLs a moment ago was listed as abandoned")
	}
	if !listed[stale] {
		t.Error("a run whose last upload activity is seven hours old was not listed")
	}
	if !listed[silent] {
		t.Error("a run with no upload activity that started eight hours ago was not listed")
	}
}

func TestIntegrationBackupRunUploadedSizeAndUsage(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	jobID, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: "h1", Schedule: "manual", RetentionCount: 3, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}
	t.Cleanup(func() { st.DeleteBackupJob(jobID) })

	running, err := st.CreateBackupRun(&models.BackupRun{JobID: jobID, Status: "running", StorageKey: "backups/h1/running.tar.gz"})
	if err != nil {
		t.Fatalf("CreateBackupRun: %v", err)
	}
	if run, err := st.GetBackupRun(running); err != nil || run.UploadedBytes != nil {
		t.Fatalf("new run: %+v, %v; want no uploaded size", run, err)
	}
	if stored, err := st.SetBackupRunUploaded(running, 4096); err != nil || !stored {
		t.Fatalf("SetBackupRunUploaded on a running run = %v, %v; want stored", stored, err)
	}
	if run, err := st.GetBackupRun(running); err != nil || run.UploadedBytes == nil || *run.UploadedBytes != 4096 {
		t.Fatalf("GetBackupRun after SetBackupRunUploaded: %+v, %v", run, err)
	}

	closed, err := st.CreateBackupRun(&models.BackupRun{JobID: jobID, Status: "running", StorageKey: "backups/h1/closed.tar.gz"})
	if err != nil {
		t.Fatalf("CreateBackupRun: %v", err)
	}
	if err := st.UpdateBackupRunStatus(closed, "failed", "reaped", 700, "backups/h1/closed.tar.gz", time.Now()); err != nil {
		t.Fatalf("UpdateBackupRunStatus: %v", err)
	}
	if stored, err := st.SetBackupRunUploaded(closed, 4096); err != nil || stored {
		t.Fatalf("SetBackupRunUploaded on a failed run = %v, %v; want refused", stored, err)
	}

	// A running run carrying a huge node-reported progress size is not usage;
	// a failed run with a size (an archive the reaper found) still is.
	if err := st.UpdateBackupRunStatus(running, "running", "", 900<<30, "backups/h1/running.tar.gz", time.Time{}); err != nil {
		t.Fatalf("progress write: %v", err)
	}
	used, err := st.BackupBytesByOwner(f.user.ID)
	if err != nil {
		t.Fatalf("BackupBytesByOwner: %v", err)
	}
	if used != 700 {
		t.Fatalf("usage = %d, want 700 (the failed run's found archive, never the running run's progress)", used)
	}
	tenants, err := st.TenantBackupBytes()
	if err != nil {
		t.Fatalf("TenantBackupBytes: %v", err)
	}
	if got := tenants[f.user.ID]; got != 0 {
		// The fixture's node has no owner, so nothing is a tenant's; the query
		// still has to run with the new clause.
		t.Fatalf("TenantBackupBytes[%s] = %d, want 0 for a server on a platform node", f.user.ID, got)
	}
}
