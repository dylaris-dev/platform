package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// A skipped slot is recorded on its own columns and leaves last_run_at alone;
// the jobs list reports what each job keeps (successful runs only).
func TestIntegrationBackupSkipAndKept(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	job, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "every 1h", RetentionCount: 3, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}

	next := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := st.SetBackupJobSkipped(job, time.Now(), "allowance", next); err != nil {
		t.Fatalf("SetBackupJobSkipped: %v", err)
	}
	got, err := st.GetBackupJob(job)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastRunAt != nil || got.LastSkipAt == nil || got.LastSkipReason != "allowance" || got.NextRunAt == nil || !got.NextRunAt.Equal(next) {
		t.Fatalf("job = lastRun %v lastSkip %v reason %q next %v", got.LastRunAt, got.LastSkipAt, got.LastSkipReason, got.NextRunAt)
	}

	for i, status := range []string{"success", "success", "failed"} {
		id, err := st.CreateBackupRun(&models.BackupRun{JobID: job, Status: "running", StorageKey: uniqueName("k_")})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateBackupRunStatus(id, status, "", int64(100*(i+1)), "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	kept, err := st.BackupKeptByJob(f.server.ID)
	if err != nil {
		t.Fatalf("BackupKeptByJob: %v", err)
	}
	if k := kept[job]; k.Runs != 2 || k.Bytes != 300 {
		t.Errorf("kept = %+v, want 2 runs / 300 bytes", k)
	}
}

// java_heap_used round-trips, NULL as nil.
func TestIntegrationStatsJavaHeap(t *testing.T) {
	_, st := integrationDB(t)
	uuid := uniqueName("srv_")
	heap := int64(2048)
	now := time.Now().Truncate(time.Second)
	if err := st.InsertStatsBatch([]models.ServerStatRow{
		{Time: now.Add(-time.Minute), ServerUUID: uuid, JavaHeapUsed: &heap},
		{Time: now, ServerUUID: uuid},
	}); err != nil {
		t.Fatalf("InsertStatsBatch: %v", err)
	}
	rows, err := st.GetStatsHistory(uuid, now.Add(-time.Hour))
	if err != nil || len(rows) != 2 {
		t.Fatalf("GetStatsHistory = %d rows, err %v", len(rows), err)
	}
	if rows[0].JavaHeapUsed == nil || *rows[0].JavaHeapUsed != 2048 || rows[1].JavaHeapUsed != nil {
		t.Errorf("heaps = %v, %v; want 2048, nil", rows[0].JavaHeapUsed, rows[1].JavaHeapUsed)
	}
}
