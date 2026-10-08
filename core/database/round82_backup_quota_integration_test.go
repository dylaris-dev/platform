package database

import (
	"testing"

	"dylaris-core/models"
)

// Core completes an upload before the node reports, and the run stays
// "running" until it does. Not counting those let a customer's node withhold
// its reports and stack completed archives past the allowance, each run
// passing the check alone; the reaper later closed them all as success.
func TestIntegrationBackupAllowanceCountsCompletedUnreportedRuns(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	job := &models.BackupJob{
		ServerID: f.server.ID, Name: uniqueName("job_"), Schedule: "manual",
		IncludePatterns: []string{}, ExcludePatterns: []string{}, RetentionCount: 3, Enabled: true,
	}
	jobID, err := st.CreateBackupJob(job)
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}
	t.Cleanup(func() { st.DeleteBackupJob(jobID) })

	running := func(uploaded int64) {
		t.Helper()
		id, err := st.CreateBackupRun(&models.BackupRun{JobID: jobID, Status: "running", StorageKey: uniqueName("k_")})
		if err != nil {
			t.Fatalf("CreateBackupRun: %v", err)
		}
		if uploaded > 0 {
			if ok, err := st.SetBackupRunUploaded(id, uploaded); err != nil || !ok {
				t.Fatalf("SetBackupRunUploaded: %v %v", ok, err)
			}
		}
	}
	running(500) // completed, report withheld
	running(0)   // still uploading: its parts are checked on their own

	billed, err := st.BackupBytesByOwner(f.user.ID)
	if err != nil {
		t.Fatalf("BackupBytesByOwner: %v", err)
	}
	if billed != 500 {
		t.Errorf("billed = %d, want 500 (the completed upload, not the one in flight)", billed)
	}
}
