package services

import (
	"encoding/json"
	"testing"

	"dylaris-core/models"
)

// A manifest that recorded the answer is the answer. A full run whose job got a
// pattern later became "selective": an old node refused the restore of a
// complete backup, and a new one overlaid it, so the rollback kept every file
// written since (R40-05). Only a manifest that says nothing falls back to the
// job's patterns.
func TestPartialBackupRun_RecordedAnswerWins(t *testing.T) {
	selective := &models.BackupJob{ExcludePatterns: []string{"logs/**"}}
	tests := []struct {
		name     string
		manifest string
		job      *models.BackupJob
		want     bool
	}{
		{"recorded full, job got a pattern later", `{"schema":1,"partial":false}`, selective, false},
		{"recorded partial, job cleared later", `{"schema":1,"partial":true}`, &models.BackupJob{}, true},
		{"not recorded, selective job", `{"schema":1}`, selective, true},
		{"not recorded, full job", `{"schema":1}`, &models.BackupJob{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PartialBackupRun(&models.BackupRun{Manifest: tt.manifest}, tt.job); got != tt.want {
				t.Errorf("PartialBackupRun = %v, want %v", got, tt.want)
			}
		})
	}
}

// Both writers record a full run as such, so the field is present either way.
func TestBackupManifestRecordsAFullRun(t *testing.T) {
	m := &BackupManifest{Schema: BackupManifestSchema}
	RecordBackupManifestPartial(m, &models.BackupJob{})
	var got map[string]any
	if err := json.Unmarshal(EncodeBackupManifest(m), &got); err != nil {
		t.Fatal(err)
	}
	if v, ok := got["partial"]; !ok || v != false {
		t.Errorf("partial = %v (present %v), want a recorded false", v, ok)
	}
}

// A selective archive left the mod jars and the loader out, so applying its
// rows to a new server listed mods that are not on disk (R40-06). Same guard as
// restoreMods/restoreInstalls on an overlaid run.
func TestApplyImportedManifest_PartialArchiveWritesNoRows(t *testing.T) {
	tests := []struct {
		name    string
		partial string
		want    bool
	}{
		{"partial", `,"partial":true`, false},
		{"recorded full", `,"partial":false`, true},
		{"not recorded", ``, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, _ := json.Marshal(scopedManifest())
			raw := string(base[:len(base)-1]) + tt.partial + "}"
			m, ok := DecodeBackupManifest(raw)
			if !ok {
				t.Fatalf("manifest %s does not decode", raw)
			}
			st := &fakeImportStore{}
			ApplyImportedManifest(st, targetServer(), "main", m)
			if got := len(st.installs) > 0; got != tt.want {
				t.Errorf("install written = %v, want %v", got, tt.want)
			}
			if got := len(st.mods) > 0; got != tt.want {
				t.Errorf("mods written = %v, want %v", got, tt.want)
			}
		})
	}
}
