package backup

import (
	"context"
	"strings"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/services"
)

// A restore with overwrite over the database Core runs on replaced the live
// platform with the snapshot. It is refused before anything connects.
func TestAPlatformRestoreNeverTargetsTheLiveDatabase(t *testing.T) {
	h := &PlatformBackupHandler{state: &handlers.AppState{PlatformDB: services.PGConn{Host: "db", Port: "5432", Name: "dylaris"}}}
	req := restoreRequest{
		Components: services.PlatformRestoreSelection{Database: true},
		Target:     restoreTargetRequest{Host: "DB", DBName: "dylaris", User: "u"},
		Overwrite:  true,
	}
	if _, _, err := h.restorer(context.Background(), req); err == nil || !strings.Contains(err.Error(), "running on") {
		t.Fatalf("err = %v, want the live-database refusal", err)
	}
}

// The rest of package C, named per site.
func TestBackupAuditCWiring(t *testing.T) {
	for _, c := range []struct{ file, fn, want, why string }{
		{"platform_backup.go", "RunJob", "context.WithoutCancel(r.Context())", "a closed tab cancels a manual platform backup"},
		{"platform_restore.go", "runRestore", "context.WithoutCancel(r.Context())", "a closed tab cancels a platform restore"},
		{"backup.go", "UpdateJob", "if next.Schedule != job.Schedule || (next.Enabled && !job.Enabled) || job.NextRunAt == nil {", "every edit restarts the schedule"},
		{"backup.go", "DownloadRun", "backupDownloadURLTTL", "a backup download link lives an hour"},
	} {
		if !strings.Contains(funcBody(t, c.file, c.fn), c.want) {
			t.Errorf("%s: %s", c.fn, c.why)
		}
	}
}
