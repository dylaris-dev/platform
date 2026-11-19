package services

import (
	"encoding/json"
	"testing"

	"dylaris-core/models"
)

// A selective run is restored as an overlay: the files it left out stay as they
// are, so the mod and install rows that describe them must stay too. Replaying
// the manifest's rows dropped the row of every mod installed after the backup
// while its jar stayed on disk (R39-03).
func TestRestoreRows_SelectiveRunLeavesTheRowsAlone(t *testing.T) {
	build := func(partial bool) string {
		b, _ := json.Marshal(&BackupManifest{
			Schema:   BackupManifestSchema,
			Partial:  partial,
			Installs: []models.SubServerInstall{{SubServerName: "survival", InstallerType: "paper"}},
			Mods:     []BackupManifestMods{{SubServer: "survival", Mods: []BackupManifestMod{}}},
		})
		return string(b)
	}
	tests := []struct {
		name      string
		manifest  string
		job       *models.BackupJob
		wantTouch bool
	}{
		{"full run", build(false), nil, true},
		{"partial manifest", build(true), nil, false},
		{"job with include patterns", build(false), &models.BackupJob{IncludePatterns: []string{"world/**"}}, false},
		{"job with exclude patterns", build(false), &models.BackupJob{ExcludePatterns: []string{"logs/**"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &installsFakeStore{manifestFakeStore: manifestFakeStore{run: &models.BackupRun{ID: 1, JobID: 4, Manifest: tt.manifest}, job: tt.job}}
			b := &BackupScheduler{store: st}
			b.restoreInstalls(1, 7)
			b.restoreMods(1, 7)
			if got := len(st.upserted) > 0; got != tt.wantTouch {
				t.Errorf("installs written = %v, want %v", got, tt.wantTouch)
			}
			if got := st.replaceCalls > 0; got != tt.wantTouch {
				t.Errorf("mods replaced = %v, want %v", got, tt.wantTouch)
			}
		})
	}
}

// The manifest records the patterns per run, because a job's patterns can be
// cleared after it ran and the run's archive stays partial.
func TestPartialBackupRun(t *testing.T) {
	partial, _ := json.Marshal(&BackupManifest{Schema: BackupManifestSchema, Partial: true})
	full, _ := json.Marshal(&BackupManifest{Schema: BackupManifestSchema})
	tests := []struct {
		name     string
		manifest string
		job      *models.BackupJob
		want     bool
	}{
		{"no manifest, full job", "", &models.BackupJob{}, false},
		{"no manifest, include", "", &models.BackupJob{IncludePatterns: []string{"world/**"}}, true},
		{"no manifest, exclude", "", &models.BackupJob{ExcludePatterns: []string{"logs/**"}}, true},
		{"partial manifest, job cleared", string(partial), &models.BackupJob{}, true},
		{"full manifest, full job", string(full), &models.BackupJob{}, false},
		{"unreadable manifest, nil job", "{x", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PartialBackupRun(&models.BackupRun{Manifest: tt.manifest}, tt.job); got != tt.want {
				t.Errorf("PartialBackupRun = %v, want %v", got, tt.want)
			}
		})
	}
}
