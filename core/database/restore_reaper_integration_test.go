package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// The reaper lists only restores still open past the cutoff, and closing one
// never overwrites a result that arrived in the meantime.
func TestIntegrationAbandonedRestoresAreListedAndClosedOnlyWhileOpen(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)
	job, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "manual", RetentionCount: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateBackupRun(&models.BackupRun{JobID: job, Status: "running", StorageKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(status string, age time.Duration) int {
		id, err := st.CreateBackupRestore(&models.BackupRestore{RunID: run, ServerID: f.server.ID, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE backup_restores SET requested_at = NOW() - $1::interval WHERE id = $2`, age.String(), id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldQueued := mk("queued", 7*time.Hour)
	oldRunning := mk("running", 8*time.Hour)
	mk("queued", time.Hour)
	mk("success", 9*time.Hour)

	got, err := st.ListAbandonedBackupRestores(time.Now().Add(-6*time.Hour), 25)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int]bool{}
	for _, r := range got {
		ids[r.ID] = true
		if r.ID == oldQueued && (r.ServerUUID != f.server.UUID || r.NodeID != f.server.NodeID) {
			t.Errorf("server uuid %q, want %q", r.ServerUUID, f.server.UUID)
		}
	}
	if len(ids) != 2 || !ids[oldQueued] || !ids[oldRunning] {
		t.Fatalf("listed %v, want only the two old open restores", got)
	}

	// The node's result lands between the list and the close.
	if err := st.UpdateBackupRestoreStatus(oldRunning, "success", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if closed, err := st.CloseAbandonedBackupRestore(oldRunning, "reaped", time.Now()); err != nil || closed {
		t.Fatalf("closed a restore that had succeeded: %v %v", closed, err)
	}
	if r, _ := st.GetBackupRestore(oldRunning); r.Status != "success" {
		t.Fatalf("status %q, want the node's success to stand", r.Status)
	}
	if closed, err := st.CloseAbandonedBackupRestore(oldQueued, "reaped", time.Now()); err != nil || !closed {
		t.Fatalf("an open abandoned restore was not closed: %v %v", closed, err)
	}
	if r, _ := st.GetBackupRestore(oldQueued); r.Status != "failed" || r.CompletedAt == nil || r.ErrorMessage != "reaped" {
		t.Fatalf("closed restore = %+v", r)
	}
}
