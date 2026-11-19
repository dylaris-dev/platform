package services

import (
	"context"
	"strings"
	"sync"
	"testing"

	"dylaris-core/models"
)

// movedFakeStore answers a server's jobs and their newest run from the
// transfer fake's maps.
type movedFakeStore struct{ *transferFakeStore }

func (f movedFakeStore) ListBackupJobs(serverID int) ([]models.BackupJob, error) {
	var out []models.BackupJob
	for _, j := range f.jobs {
		if j.ServerID == serverID {
			out = append(out, *j)
		}
	}
	return out, nil
}

func (f movedFakeStore) ListBackupRuns(jobID, _ int) ([]models.BackupRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []models.BackupRun
	for _, r := range f.runs {
		if r.JobID == jobID {
			out = append(out, *r)
		}
	}
	return out, nil
}

// R35-07: a spooled upload outlives the writer hold, so a server could move to
// another node mid-upload. Core then refused the old node's part requests and
// dropped its reports, and the run stayed "running" until the 6h reaper,
// refusing every backup of the server on its new node meanwhile. The cutover
// now closes it.
func TestCloseRunsOfMovedServer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		uploaded   bool
		wantStatus string
		wantAbort  bool
	}{
		{"an unfinished upload is failed and discarded", "running", false, "failed", true},
		{"an upload Core completed is kept", "running", true, "success", false},
		{"a finished run is left alone", "success", false, "success", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTransferFakeStore()
			st.runs[1].Status = tc.status
			st.runs[1].UploadID, st.runs[1].PartSize = "upload-1", backupPartSize
			if tc.uploaded {
				n := int64(42)
				st.runs[1].UploadedBytes = &n
			}
			prov := &fakeMultipartStorage{}
			b := transferScheduler(st, prov, nil)
			b.store = movedFakeStore{st}

			b.CloseRunsOfMovedServer(context.Background(), 100)

			if got := st.runs[1].Status; got != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got, tc.wantStatus)
			}
			if tc.wantStatus == "failed" {
				if len(st.updates) != 1 || !strings.Contains(st.updates[0].message, "moved to another node") {
					t.Errorf("updates = %+v, want one failure that says the server moved", st.updates)
				}
			}
			if got := len(st.aborts(prov)) > 0; got != tc.wantAbort {
				t.Errorf("aborted = %v, want %v", st.aborts(prov), tc.wantAbort)
			}
		})
	}
}

// The cutover is what closes them, on a fresh move and on a re-run that finds
// the server on its target already.
func TestAMoveClosesTheBackupRunsTheSourceCannotFinish(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		onNode int
		req    MigrationRequest
	}{
		{"a move", "stopped", 1, MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, Reason: "manual"}},
		{"a re-run after the cutover", "migrating", 2, MigrationRequest{ServerID: 5, TargetNodeID: 2, SourceNodeID: 1, PreStatus: "stopped", Reason: "manual"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, fs, _, _ := newFlow(t, tc.status)
			fs.srv.NodeID = tc.onNode
			var mu sync.Mutex
			var closed []int
			o.SetMovedBackups(func(_ context.Context, serverID int) {
				mu.Lock()
				defer mu.Unlock()
				closed = append(closed, serverID)
			})
			o.Migrate(context.Background(), tc.req)
			mu.Lock()
			defer mu.Unlock()
			if len(closed) != 1 || closed[0] != 5 {
				t.Fatalf("backup runs closed for servers %v, want once for server 5", closed)
			}
		})
	}
}
