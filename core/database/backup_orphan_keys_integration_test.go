package database

import (
	"testing"

	"dylaris-core/models"
)

// The orphan scan deletes whatever this set does NOT contain, so every row that
// names an archive must reach it: a run with no storage recorded anywhere, a run
// still writing, a failed run whose object may exist, and a platform bundle.
func TestIntegrationReferencedBackupKeysCoverEveryRun(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	other, err := st.CreateBackupStorage(s3Storage(uniqueName("orph_st_"), nil, false))
	if err != nil {
		t.Fatalf("CreateBackupStorage: %v", err)
	}
	t.Cleanup(func() { st.DeleteBackupStorage(other) })

	jobNull, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_null_"), Schedule: "manual", RetentionCount: 3, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}
	jobOther, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_other_"), Schedule: "manual", RetentionCount: 3, Enabled: true, StorageID: &other})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}

	tag := uniqueName("orphkeys_")
	nullKey := "backups/" + tag + "/null.tar.gz"
	runningKey := "backups/" + tag + "/running.tar.gz"
	failedKey := "backups/" + tag + "/failed.tar.gz"
	otherKey := "backups/" + tag + "/other.tar.gz"
	for _, spec := range []struct {
		job    int
		status string
		key    string
		sid    *int
	}{
		{jobNull, "success", nullKey, nil},
		{jobNull, "running", runningKey, nil},
		{jobNull, "failed", failedKey, nil},
		{jobOther, "success", otherKey, &other},
		{jobNull, "failed", "", nil},
	} {
		if _, err := st.CreateBackupRun(&models.BackupRun{JobID: spec.job, Status: spec.status, StorageKey: spec.key, StorageID: spec.sid}); err != nil {
			t.Fatalf("CreateBackupRun: %v", err)
		}
	}

	pjob, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{
		Name: uniqueName("pj_"), Schedule: "manual", RetentionCount: 3, Enabled: true,
		Selection: models.PlatformBackupSelection{Database: true},
	})
	if err != nil {
		t.Fatalf("CreatePlatformBackupJob: %v", err)
	}
	t.Cleanup(func() { st.DeletePlatformBackupJob(pjob) })
	prun, err := st.CreatePlatformBackupRun(pjob, nil)
	if err != nil {
		t.Fatalf("CreatePlatformBackupRun: %v", err)
	}
	platformKey := "platform-backups/" + tag + "/bundle.dylaris-bundle"
	if err := st.FinishPlatformBackupRun(prun, "success", 1, platformKey, "", nil); err != nil {
		t.Fatalf("FinishPlatformBackupRun: %v", err)
	}

	keys, err := st.ListReferencedBackupKeys()
	if err != nil {
		t.Fatalf("ListReferencedBackupKeys: %v", err)
	}
	for _, want := range []string{nullKey, runningKey, failedKey, otherKey, platformKey} {
		if !keys[want] {
			t.Errorf("%q is named by a row but missing from the set; the scan would offer it for deletion", want)
		}
	}
	if keys[""] {
		t.Error("the empty key of a run that wrote nothing is in the set")
	}
	if keys["backups/"+tag+"/never-written.tar.gz"] {
		t.Error("a key no row names is reported as referenced")
	}
}
