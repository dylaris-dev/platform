package database

import (
	"testing"

	"dylaris-core/models"
)

// The zone a schedule is read in round-trips through both job tables, and a job
// written without one is stored as UTC, which is what an empty zone meant.
func TestIntegrationBackupJobTimezone(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	id, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "daily 03:00", Timezone: "Europe/Berlin", RetentionCount: 3, Enabled: true})
	if err != nil {
		t.Fatalf("CreateBackupJob: %v", err)
	}
	job, err := st.GetBackupJob(id)
	if err != nil || job.Timezone != "Europe/Berlin" || job.Schedule != "daily 03:00" {
		t.Fatalf("job = %+v (%v), want daily 03:00 in Europe/Berlin", job, err)
	}
	job.Timezone = "America/New_York"
	if err := st.UpdateBackupJob(job); err != nil {
		t.Fatal(err)
	}
	if job, _ = st.GetBackupJob(id); job.Timezone != "America/New_York" {
		t.Fatalf("zone after update = %q", job.Timezone)
	}

	id, err = st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "every 6h", RetentionCount: 3})
	if err != nil {
		t.Fatal(err)
	}
	if job, _ = st.GetBackupJob(id); job.Timezone != "UTC" {
		t.Fatalf("zone of a job written without one = %q, want UTC", job.Timezone)
	}

	pid, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{Name: uniqueName("p_"), Schedule: "weekly sun 04:00", Timezone: "Asia/Tokyo", Selection: models.PlatformBackupSelection{Database: true}})
	if err != nil {
		t.Fatal(err)
	}
	pj, err := st.GetPlatformBackupJob(pid)
	if err != nil || pj.Timezone != "Asia/Tokyo" {
		t.Fatalf("platform job = %+v (%v)", pj, err)
	}
	pj.Timezone = ""
	if err := st.UpdatePlatformBackupJob(pj); err != nil {
		t.Fatal(err)
	}
	if pj, _ = st.GetPlatformBackupJob(pid); pj.Timezone != "UTC" {
		t.Fatalf("platform zone after clearing = %q, want UTC", pj.Timezone)
	}
}
