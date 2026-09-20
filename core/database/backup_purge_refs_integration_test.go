package database

import (
	"testing"

	"dylaris-core/models"
)

// Against a real Postgres, because what is being checked IS the SQL: the join
// down to the runs, the COALESCE that prefers the run's own storage over the
// job's, and the owner that decides which storage chain the id resolves in.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.

// The lookup a delete depends on: every archive of every schedule of the
// server, read BEFORE the cascade removes the only rows that name them.
func TestIntegrationBackupRunRefsForServersFindsEveryArchive(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	jobA, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_a_"), Schedule: "manual", RetentionCount: 3, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}
	jobB, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_b_"), Schedule: "manual", RetentionCount: 3, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}
	for _, spec := range []struct {
		job int
		key string
	}{{jobA, "k-a1"}, {jobA, "k-a2"}, {jobB, "k-b1"}} {
		if _, err := st.CreateBackupRun(&models.BackupRun{JobID: spec.job, Status: "success", StorageKey: spec.key}); err != nil {
			t.Fatalf("CreateBackupRun: %v", err)
		}
	}

	refs, err := st.ListBackupRunRefsForServers([]int{f.server.ID})
	if err != nil {
		t.Fatalf("ListBackupRunRefsForServers: %v", err)
	}
	keys := map[string]bool{}
	for _, r := range refs {
		keys[r.StorageKey] = true
		if r.OwnerID != f.user.ID {
			t.Errorf("run %d carries owner %q, want the server's owner %q - the wrong owner resolves the storage in the wrong chain", r.RunID, r.OwnerID, f.user.ID)
		}
	}
	for _, want := range []string{"k-a1", "k-a2", "k-b1"} {
		if !keys[want] {
			t.Errorf("archive %q was not reported; it would be left in the bucket", want)
		}
	}

	// One schedule's worth, for the schedule delete.
	only, err := st.ListBackupRunRefsForJob(jobB)
	if err != nil {
		t.Fatalf("ListBackupRunRefsForJob: %v", err)
	}
	if len(only) != 1 || only[0].StorageKey != "k-b1" {
		t.Fatalf("refs for one job = %+v, want only k-b1", only)
	}

	// No servers must mean no archives, never "every archive".
	none, err := st.ListBackupRunRefsForServers(nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("empty server list returned %d ref(s), err=%v", len(none), err)
	}
}

// The run's own storage wins over the job's: an archive written before the
// schedule was pointed elsewhere still lives where it was written, and deleting
// it against the job's current storage removes nothing while the row goes.
func TestIntegrationBackupRunRefsPreferTheRunsOwnStorage(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	jobStorage, err := st.CreateBackupStorage(s3Storage(uniqueName("job_st_"), nil, false))
	if err != nil {
		t.Fatalf("CreateBackupStorage: %v", err)
	}
	t.Cleanup(func() { st.DeleteBackupStorage(jobStorage) })
	runStorage, err := st.CreateBackupStorage(s3Storage(uniqueName("run_st_"), nil, false))
	if err != nil {
		t.Fatalf("CreateBackupStorage: %v", err)
	}
	t.Cleanup(func() { st.DeleteBackupStorage(runStorage) })

	jobID, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "manual", RetentionCount: 3, Enabled: true, StorageID: &jobStorage})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}
	moved, err := st.CreateBackupRun(&models.BackupRun{JobID: jobID, Status: "success", StorageKey: "k-moved", StorageID: &runStorage})
	if err != nil {
		t.Fatalf("CreateBackupRun: %v", err)
	}
	if _, err := st.CreateBackupRun(&models.BackupRun{JobID: jobID, Status: "success", StorageKey: "k-inherited"}); err != nil {
		t.Fatalf("CreateBackupRun: %v", err)
	}

	refs, err := st.ListBackupRunRefsForJob(jobID)
	if err != nil {
		t.Fatalf("ListBackupRunRefsForJob: %v", err)
	}
	for _, r := range refs {
		if r.StorageID == nil {
			t.Fatalf("run %d reported no storage at all", r.RunID)
		}
		want := jobStorage
		if r.RunID == moved {
			want = runStorage
		}
		if *r.StorageID != want {
			t.Errorf("run %d (%s) resolves to storage %d, want %d", r.RunID, r.StorageKey, *r.StorageID, want)
		}
	}
}
