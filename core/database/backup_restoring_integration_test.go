package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// Retention and a manual delete removed an archive a restore was still
// waiting on, after the node had stopped the server for it.
func TestIntegrationARunBeingRestoredIsKeptByRetention(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	job, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "manual", RetentionCount: 1, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}
	var runs []int
	for i, key := range []string{"old", "mid", "new"} {
		id, err := st.CreateBackupRun(&models.BackupRun{JobID: job, Status: "running", StorageKey: key})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateBackupRunStatus(id, "success", "", 1, key, time.Now().Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, id)
		time.Sleep(10 * time.Millisecond) // distinct started_at
	}
	if _, err := st.CreateBackupRestore(&models.BackupRestore{RunID: runs[0], ServerID: f.server.ID, Status: "queued"}); err != nil {
		t.Fatalf("CreateBackupRestore: %v", err)
	}

	if busy, err := st.BackupRunRestoring(runs[0]); err != nil || !busy {
		t.Fatalf("BackupRunRestoring(old) = %v, %v; want true", busy, err)
	}
	if busy, err := st.BackupRunRestoring(runs[1]); err != nil || busy {
		t.Fatalf("BackupRunRestoring(mid) = %v, %v; want false", busy, err)
	}
	pruned, err := st.PruneOldBackupRuns(job, 1)
	if err != nil {
		t.Fatalf("PruneOldBackupRuns: %v", err)
	}
	if len(pruned) != 1 || pruned[0].ID != runs[1] {
		t.Fatalf("pruned %+v, want only the middle run - the oldest is being restored", pruned)
	}
}
