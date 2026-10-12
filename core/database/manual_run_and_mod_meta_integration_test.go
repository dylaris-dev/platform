package database

import (
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// A manual run records itself and keeps a planned slot; a job with no next run
// yet is armed by it. It used to move the schedule to click time + interval.
func TestIntegrationManualBackupRunKeepsTheSchedule(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	planned := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	armed, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "every 6h", RetentionCount: 3, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}
	if err := st.SetBackupJobScheduled(armed, time.Now().Add(-3*time.Hour), planned); err != nil {
		t.Fatal(err)
	}
	unarmed, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "every 6h", RetentionCount: 3, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	byClick := now.Add(6 * time.Hour)
	for _, id := range []int{armed, unarmed} {
		if err := st.SetBackupJobRanByHand(id, now, &byClick); err != nil {
			t.Fatalf("SetBackupJobRanByHand: %v", err)
		}
	}
	got, _ := st.GetBackupJob(armed)
	if got.NextRunAt == nil || !got.NextRunAt.Equal(planned) || got.LastRunAt == nil || !got.LastRunAt.Equal(now) {
		t.Errorf("armed job: next %v (want %v), last %v (want %v)", got.NextRunAt, planned, got.LastRunAt, now)
	}
	got, _ = st.GetBackupJob(unarmed)
	if got.NextRunAt == nil || !got.NextRunAt.Equal(byClick) {
		t.Errorf("unarmed job: next %v, want armed to %v", got.NextRunAt, byClick)
	}
}

func TestIntegrationManualPlatformBackupRunKeepsTheSchedule(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	sel := models.PlatformBackupSelection{Database: true}
	planned := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Second)
	armed, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{Name: "armed", Schedule: "every 1d", Selection: sel, Enabled: true, NextRunAt: &planned})
	if err != nil {
		t.Fatal(err)
	}
	unarmed, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{Name: "unarmed", Schedule: "every 1d", Selection: sel, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	byClick := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	for _, id := range []int{armed, unarmed} {
		if err := st.SetPlatformBackupJobRanByHand(id, &byClick); err != nil {
			t.Fatalf("SetPlatformBackupJobRanByHand: %v", err)
		}
	}
	got, _ := st.GetPlatformBackupJob(armed)
	if got.NextRunAt == nil || !got.NextRunAt.Equal(planned) || got.LastRunAt == nil {
		t.Errorf("armed job: next %v (want %v), last %v", got.NextRunAt, planned, got.LastRunAt)
	}
	got, _ = st.GetPlatformBackupJob(unarmed)
	if got.NextRunAt == nil || !got.NextRunAt.Equal(byClick) {
		t.Errorf("unarmed job: next %v, want armed to %v", got.NextRunAt, byClick)
	}
}

// The project title and slug land only on a row that has no slug, so a heal
// racing an install never overwrites what the install wrote.
func TestIntegrationModProjectMetaFillsOnlyMissingSlugs(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	identified := &models.ServerMod{ServerID: f.server.ID, SubServerName: "main", ModrinthProjectID: "l6YH9Als", ModrinthVersionID: "v1", Title: "spark 1.10 for fabric", FileName: "spark.jar", TargetDir: "mods"}
	installed := &models.ServerMod{ServerID: f.server.ID, SubServerName: "main", ModrinthProjectID: "AANobbMI", ModrinthProjectSlug: "sodium", ModrinthVersionID: "v2", Title: "Sodium", FileName: "sodium.jar", TargetDir: "mods"}
	var ids []int
	for _, m := range []*models.ServerMod{identified, installed} {
		id, err := st.UpsertServerMod(m)
		if err != nil {
			t.Fatalf("UpsertServerMod: %v", err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if err := st.SetServerModProjectMeta(id, "healed", "Healed"); err != nil {
			t.Fatalf("SetServerModProjectMeta: %v", err)
		}
	}
	mods, err := st.ListServerMods(f.server.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range mods {
		got[m.ModrinthProjectID] = m.ModrinthProjectSlug + "|" + m.Title
	}
	if got["l6YH9Als"] != "healed|Healed" || got["AANobbMI"] != "sodium|Sodium" {
		t.Fatalf("rows = %v", got)
	}
}
