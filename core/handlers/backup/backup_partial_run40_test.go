package backup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
)

// The panel's restore warning read only the job's current patterns, so it
// promised a full replace for a partial run whose job was cleared and an
// overlay for a full run whose job got a pattern later (R40-05). The run list
// carries Core's own answer.
func TestListRuns_SaysWhetherARestoreOverlays(t *testing.T) {
	yes, no := true, false
	partial, _ := json.Marshal(services.BackupManifest{Schema: services.BackupManifestSchema, Partial: &yes})
	full, _ := json.Marshal(services.BackupManifest{Schema: services.BackupManifestSchema, Partial: &no})
	tests := []struct {
		name     string
		exclude  []string
		manifest string
		want     bool
	}{
		{"partial run, job cleared", nil, string(partial), true},
		{"full run, job now selective", []string{"logs/**"}, string(full), false},
		{"no manifest, selective job", []string{"logs/**"}, "", true},
		{"no manifest, full job", nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, fs, _, _ := presignedOnlyHandler(t, currentNodeVersion, models.BackupRun{ID: 1, JobID: 10, Status: "success", StorageKey: "backups/k.tar.gz", Manifest: tt.manifest})
			h.d.Store = &selectiveJobStore{presignedOnlyStore: fs, exclude: tt.exclude}
			rw := httptest.NewRecorder()
			h.ListRuns(rw, backupJobRequest(http.MethodGet, "/api/backup-jobs/10/runs", "", map[string]string{"jobId": "10"}))
			if rw.Code != http.StatusOK {
				t.Fatalf("status = %d (body %s)", rw.Code, rw.Body.String())
			}
			var body struct {
				Runs []struct {
					Partial *bool `json:"partial"`
				} `json:"runs"`
			}
			if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil || len(body.Runs) != 1 {
				t.Fatalf("body %s: %v", rw.Body.String(), err)
			}
			if p := body.Runs[0].Partial; p == nil || *p != tt.want {
				t.Errorf("partial = %v, want %v", p, tt.want)
			}
		})
	}
}
