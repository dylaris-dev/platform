package database

import (
	"errors"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// A storage a backup or a schedule still names is not deleted: the reference
// is ON DELETE SET NULL, and a NULL storage resolves to the platform default,
// so a tenant's archives were billed as ours and restored from a bucket that
// never held them.
func TestIntegrationBackupStorageInUseIsNotDeleted(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	newStorage := func() int {
		t.Helper()
		id, err := st.CreateBackupStorage(s3Storage(uniqueName("st_"), &f.user.ID, false))
		if err != nil {
			t.Fatalf("CreateBackupStorage: %v", err)
		}
		return id
	}
	gone := func(id int) bool {
		t.Helper()
		_, err := st.GetBackupStorage(id)
		return err != nil
	}

	for _, tc := range []struct {
		name  string
		refer func(t *testing.T, storageID int) (release func())
	}{
		{"schedule", func(t *testing.T, sid int) func() {
			id, err := st.CreateBackupJob(&models.BackupJob{
				ServerID: f.server.ID, Name: uniqueName("job_"), Schedule: "manual", StorageID: &sid,
				IncludePatterns: []string{}, ExcludePatterns: []string{}, RetentionCount: 3, Enabled: true,
			})
			if err != nil {
				t.Fatalf("CreateBackupJob: %v", err)
			}
			return func() { st.DeleteBackupJob(id) }
		}},
		{"backup", func(t *testing.T, sid int) func() {
			jobID, err := st.CreateBackupJob(&models.BackupJob{
				ServerID: f.server.ID, Name: uniqueName("job_"), Schedule: "manual",
				IncludePatterns: []string{}, ExcludePatterns: []string{}, RetentionCount: 3, Enabled: true,
			})
			if err != nil {
				t.Fatalf("CreateBackupJob: %v", err)
			}
			if _, err := st.CreateBackupRun(&models.BackupRun{JobID: jobID, Status: "success", StorageKey: uniqueName("k_"), StorageID: &sid}); err != nil {
				t.Fatalf("CreateBackupRun: %v", err)
			}
			return func() { st.DeleteBackupJob(jobID) }
		}},
		{"platform backup", func(t *testing.T, sid int) func() {
			jobID, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{Name: uniqueName("pjob_"), Schedule: "manual", RetentionCount: 3})
			if err != nil {
				t.Fatalf("CreatePlatformBackupJob: %v", err)
			}
			runID, err := st.CreatePlatformBackupRun(jobID, &sid)
			if err != nil {
				t.Fatalf("CreatePlatformBackupRun: %v", err)
			}
			// Failed, it no longer pins the storage: retention never prunes a
			// failed platform run and no route deletes one.
			return func() {
				if err := st.FinishPlatformBackupRun(runID, "failed", 0, "", "boom", nil); err != nil {
					t.Fatalf("FinishPlatformBackupRun: %v", err)
				}
			}
		}},
		{"platform schedule", func(t *testing.T, sid int) func() {
			id, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{Name: uniqueName("pjob_"), Schedule: "manual", StorageID: &sid, RetentionCount: 3})
			if err != nil {
				t.Fatalf("CreatePlatformBackupJob: %v", err)
			}
			return func() { st.DeletePlatformBackupJob(id) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sid := newStorage()
			release := tc.refer(t, sid)
			if err := st.DeleteBackupStorage(sid); !errors.Is(err, store.ErrStorageInUse) {
				t.Fatalf("delete while referenced: %v, want ErrStorageInUse", err)
			}
			if gone(sid) {
				t.Fatal("the storage was deleted while referenced")
			}
			release()
			if err := st.DeleteBackupStorage(sid); err != nil {
				t.Fatalf("delete once free: %v", err)
			}
			if !gone(sid) {
				t.Fatal("a free storage was not deleted")
			}
		})
	}
}
