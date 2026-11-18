package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// The reaper's close lands only on a run still running with no upload Core
// completed; a run finished in between keeps its state and its archive.
func TestIntegrationFailAbandonedBackupRunIsConditional(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	jobID, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "manual", RetentionCount: 3, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}
	t.Cleanup(func() { st.DeleteBackupJob(jobID) })
	newRun := func(key string) int {
		id, err := st.CreateBackupRun(&models.BackupRun{JobID: jobID, Status: "running", StorageKey: key})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	stale, completed, succeeded := newRun("stale"), newRun("completed"), newRun("succeeded")
	if ok, err := st.SetBackupRunUploaded(completed, 4096); err != nil || !ok {
		t.Fatalf("SetBackupRunUploaded = %v, %v", ok, err)
	}
	if err := st.UpdateBackupRunStatus(succeeded, "success", "", 1, "succeeded", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id         int
		want       bool
		wantStatus string
	}{{stale, true, "failed"}, {completed, false, "running"}, {succeeded, false, "success"}} {
		got, err := st.FailAbandonedBackupRun(tc.id, "abandoned", 0, time.Now())
		if err != nil || got != tc.want {
			t.Fatalf("FailAbandonedBackupRun(%d) = %v, %v; want %v", tc.id, got, err, tc.want)
		}
		run, err := st.GetBackupRun(tc.id)
		if err != nil || run.Status != tc.wantStatus {
			t.Fatalf("run %d status = %v (%v), want %s", tc.id, run, err, tc.wantStatus)
		}
	}
}

// Anonymize keeps the row and its invites, so it must take the SFTP keys and
// the pending address with it.
func TestIntegrationAnonymizeUserRemovesSFTPKeys(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)
	if _, err := st.AddSSHKey(&models.SSHKey{UserID: f.user.ID, Name: "k", PublicKey: "ssh-ed25519 AAAA", Fingerprint: uniqueName("fp_")}, 10); err != nil {
		t.Fatalf("AddSSHKey: %v", err)
	}
	if _, err := db.Exec(`UPDATE users SET pending_email = 'new@example.com' WHERE id = $1`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.AnonymizeUser(f.user.ID); err != nil {
		t.Fatalf("AnonymizeUser: %v", err)
	}
	keys, err := st.ListSSHKeysByUser(f.user.ID)
	if err != nil || len(keys) != 0 {
		t.Fatalf("ssh keys after anonymize = %d (%v), want none", len(keys), err)
	}
	var pending *string
	if err := db.QueryRow(`SELECT pending_email FROM users WHERE id = $1`, f.user.ID).Scan(&pending); err != nil || pending != nil {
		t.Fatalf("pending_email after anonymize = %v (%v), want NULL", pending, err)
	}
}
