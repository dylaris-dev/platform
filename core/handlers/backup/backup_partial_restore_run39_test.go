package backup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
)

// selectiveJobStore is presignedOnlyStore with a job that has patterns.
type selectiveJobStore struct {
	*presignedOnlyStore
	include, exclude []string
}

func (f *selectiveJobStore) GetBackupJob(id int) (*models.BackupJob, error) {
	return &models.BackupJob{ID: id, ServerID: 100, Schedule: "manual", IncludePatterns: f.include, ExcludePatterns: f.exclude}, nil
}

// A selective run holds only what its patterns matched, and a node replaces the
// restore target with the archive: restored that way it deleted everything the
// patterns left out (R39-03). Core sends such a run only as an overlay, and only
// to a node that says it can do one.
func TestRestoreRun_SelectiveRunNeedsANodeThatOverlays(t *testing.T) {
	partial, _ := json.Marshal(services.BackupManifest{Schema: services.BackupManifestSchema, Partial: true})
	tests := []struct {
		name             string
		include, exclude []string
		manifest         string
		nodeOverlays     bool
		wantCode         int
		wantOverlay      bool
	}{
		{"full run, old node", nil, nil, "", false, http.StatusOK, false},
		{"full run, new node", nil, nil, "", true, http.StatusOK, false},
		{"include patterns, old node", []string{"world/**"}, nil, "", false, http.StatusConflict, false},
		{"exclude patterns, old node", nil, []string{"logs/**"}, "", false, http.StatusConflict, false},
		{"include patterns, new node", []string{"world/**"}, nil, "", true, http.StatusOK, true},
		// The job lost its patterns after the run: the run's own manifest says.
		{"partial manifest, job now full, old node", nil, nil, string(partial), false, http.StatusConflict, false},
		{"partial manifest, job now full, new node", nil, nil, string(partial), true, http.StatusOK, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, fs, rdb, _ := presignedOnlyHandler(t, currentNodeVersion, models.BackupRun{ID: 1, JobID: 10, Status: "success", StorageKey: "backups/k.tar.gz", Manifest: tt.manifest})
			h.d.Store = &selectiveJobStore{presignedOnlyStore: fs, include: tt.include, exclude: tt.exclude}
			hb, _ := json.Marshal(services.NodeHeartbeat{ID: "node-hosting", ReleaseVersion: currentNodeVersion, RestoreOverlay: tt.nodeOverlays})
			rdb.Set(context.Background(), "dylaris:discovery:node-hosting", string(hb), 0)

			rw := httptest.NewRecorder()
			h.RestoreRun(rw, backupJobRequest(http.MethodPost, "/api/backup-runs/1/restore", "", map[string]string{"runId": "1"}))
			if rw.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", rw.Code, tt.wantCode, rw.Body.String())
			}
			cmds := queuedCommands(t, rdb)
			if tt.wantCode != http.StatusOK {
				// Refused before anything: the node stops the server first.
				if len(cmds) != 0 || len(fs.restoreUpdates) != 0 {
					t.Fatalf("commands = %v, restore updates = %v, want none", cmds, fs.restoreUpdates)
				}
				return
			}
			if len(cmds) != 1 {
				t.Fatalf("commands = %d, want 1", len(cmds))
			}
			var cmd map[string]interface{}
			if err := json.Unmarshal([]byte(cmds[0]), &cmd); err != nil {
				t.Fatal(err)
			}
			if got := cmd["overlay"] == true; got != tt.wantOverlay {
				t.Errorf("overlay = %v, want %v", cmd["overlay"], tt.wantOverlay)
			}
		})
	}
}
