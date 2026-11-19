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
	yes, no := true, false
	build := func(partial *bool) string {
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
		{"full run", build(&no), nil, true},
		{"partial manifest", build(&yes), nil, false},
		// Manifests from before the field: the job's patterns answer.
		{"job with include patterns", build(nil), &models.BackupJob{IncludePatterns: []string{"world/**"}}, false},
		{"job with exclude patterns", build(nil), &models.BackupJob{ExcludePatterns: []string{"logs/**"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &installsFakeStore{manifestFakeStore: manifestFakeStore{run: &models.BackupRun{ID: 1, JobID: 4, Manifest: tt.manifest}, job: tt.job}}
			b := &BackupScheduler{store: st}
			b.restoreInstalls(1, 7, nil)
			b.restoreMods(1, 7, nil)
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
	yes := true
	partial, _ := json.Marshal(&BackupManifest{Schema: BackupManifestSchema, Partial: &yes})
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

// The overlay decision sent to the node is the one the rows follow. Re-reading
// the job's patterns at the result flipped it when the patterns were edited
// while the restore ran: cleared patterns replaced the rows of an overlay that
// kept the live mods, an added pattern left the rows of a full swap stale
// (R41-04). A restore without a recorded decision falls back to the run.
func TestRestoreRows_FollowTheRecordedOverlayDecision(t *testing.T) {
	yes, no := true, false
	manifest, _ := json.Marshal(&BackupManifest{
		Schema:   BackupManifestSchema,
		Installs: []models.SubServerInstall{{SubServerName: "survival", InstallerType: "paper"}},
		Mods:     []BackupManifestMods{{SubServer: "survival", Mods: []BackupManifestMod{}}},
	})
	selective := &models.BackupJob{ExcludePatterns: []string{"logs/**"}}
	tests := []struct {
		name      string
		job       *models.BackupJob // the job as it reads at the result
		overlay   *bool             // what the restore recorded at dispatch
		wantTouch bool
	}{
		{"overlay sent, patterns cleared since", &models.BackupJob{}, &yes, false},
		{"full sent, pattern added since", selective, &no, true},
		{"not recorded, selective job", selective, nil, false},
		{"not recorded, full job", &models.BackupJob{}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &installsFakeStore{manifestFakeStore: manifestFakeStore{run: &models.BackupRun{ID: 1, JobID: 4, Manifest: string(manifest)}, job: tt.job}}
			b := &BackupScheduler{store: st}
			b.restoreInstalls(1, 7, tt.overlay)
			b.restoreMods(1, 7, tt.overlay)
			if got := len(st.upserted) > 0; got != tt.wantTouch {
				t.Errorf("installs written = %v, want %v", got, tt.wantTouch)
			}
			if got := st.replaceCalls > 0; got != tt.wantTouch {
				t.Errorf("mods replaced = %v, want %v", got, tt.wantTouch)
			}
		})
	}
}
