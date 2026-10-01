package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// A manual trigger started another run beside one already in progress, every
// time. The run is now created only while the job has none running, in one
// statement, so two triggers at once cannot both pass.
func TestIntegrationBackupRunStartsOnlyWhenIdle(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	job, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "manual", RetentionCount: 3, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}

	first, started, err := st.StartBackupRunIfIdle(&models.BackupRun{JobID: job, Status: "running", StorageKey: "k1"})
	if err != nil || !started {
		t.Fatalf("first run: started=%v err=%v", started, err)
	}
	if _, started, err := st.StartBackupRunIfIdle(&models.BackupRun{JobID: job, Status: "running", StorageKey: "k2"}); err != nil || started {
		t.Fatalf("second run while the first runs: started=%v err=%v, want refused", started, err)
	}
	if err := st.UpdateBackupRunStatus(first, "success", "", 1, "k1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, started, err := st.StartBackupRunIfIdle(&models.BackupRun{JobID: job, Status: "running", StorageKey: "k3"}); err != nil || !started {
		t.Fatalf("a run after the first finished: started=%v err=%v", started, err)
	}
}
