package database

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// A Core that died mid-run left a platform run "running" for good. Old ones are
// closed failed; a run still within its time is left alone.
func TestIntegrationStalePlatformRunsAreClosed(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	job, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{Name: "j", Schedule: "every 1d", Selection: models.PlatformBackupSelection{Database: true}})
	if err != nil {
		t.Fatal(err)
	}
	job2, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{Name: "j2", Schedule: "every 1d", Selection: models.PlatformBackupSelection{Database: true}})
	if err != nil {
		t.Fatal(err)
	}
	old, _ := st.CreatePlatformBackupRun(job, nil)
	fresh, _ := st.CreatePlatformBackupRun(job2, nil)
	if _, err := db.Exec(`UPDATE platform_backup_runs SET started_at = NOW() - interval '7 hours' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	n, err := st.FailStalePlatformBackupRuns(6 * time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("closed %d (%v), want 1", n, err)
	}
	status := func(id int) (s string) {
		db.QueryRow(`SELECT status FROM platform_backup_runs WHERE id = $1`, id).Scan(&s)
		return
	}
	if status(old) != "failed" || status(fresh) != "running" {
		t.Fatalf("old %q fresh %q", status(old), status(fresh))
	}
}

// A storage named by a platform job is in use; the same question a delete asks
// now also guards an edit of where it stores archives.
func TestIntegrationBackupStorageInUse(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	sid, err := st.CreateBackupStorage(&models.BackupStorage{Name: "s", Provider: "shared", Config: json.RawMessage(`{"basePath":"/b"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if used, err := st.BackupStorageInUse(sid); err != nil || used {
		t.Fatalf("fresh storage in use=%v err=%v", used, err)
	}
	if _, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{Name: "j", Schedule: "every 1d", StorageID: &sid, Selection: models.PlatformBackupSelection{Database: true}}); err != nil {
		t.Fatal(err)
	}
	if used, err := st.BackupStorageInUse(sid); err != nil || !used {
		t.Fatalf("storage named by a job in use=%v err=%v", used, err)
	}
}

// A default storage also holds the archives of jobs that name no storage.
func TestIntegrationTheDefaultStorageIsInUseByJobsThatNameNone(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	sid, err := st.CreateBackupStorage(&models.BackupStorage{Name: "d", Provider: "shared", IsDefault: true, Config: json.RawMessage(`{"basePath":"/b"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if used, _ := st.BackupStorageInUse(sid); used {
		t.Fatal("an unused default reads as in use")
	}
	f := newFixture(t, st)
	if _, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: "legacy", Schedule: "every 1d", RetentionCount: 3, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if used, err := st.BackupStorageInUse(sid); err != nil || !used {
		t.Fatalf("default with a storage-less job: in use=%v err=%v", used, err)
	}
}

// One running run per platform job: the scheduler and a manual run on another
// replica started the same job twice.
func TestIntegrationAPlatformJobRunsOnceAtATime(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	job, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{Name: "j", Schedule: "every 1d", Selection: models.PlatformBackupSelection{Database: true}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.CreatePlatformBackupRun(job, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePlatformBackupRun(job, nil); !errors.Is(err, store.ErrPlatformRunInProgress) {
		t.Fatalf("second run while the first runs: %v, want ErrPlatformRunInProgress", err)
	}
	if err := st.FinishPlatformBackupRun(first, "success", 1, "k", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePlatformBackupRun(job, nil); err != nil {
		t.Fatalf("a run after the first finished: %v", err)
	}
}
