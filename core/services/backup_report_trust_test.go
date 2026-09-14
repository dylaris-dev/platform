package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-pkg/queue"
)

// A node's result report is a claim from a machine that may be a customer's. On
// object storage Core completed the upload and measured it, so these pin that
// the report cannot choose the size, cannot turn an upload Core never completed
// into a success, and cannot leave an archive behind by reporting failure.

func TestSettleObjectStorageReport(t *testing.T) {
	uploaded := int64(300 << 30)
	tests := []struct {
		name        string
		report      string
		run         models.BackupRun
		wantApply   bool
		wantStatus  string
		wantSize    int64
		wantMessage string
	}{
		{
			name: "success takes Core's size, not the node's", report: "success",
			run:       models.BackupRun{Status: "running", UploadID: "u", UploadedBytes: &uploaded},
			wantApply: true, wantStatus: "success", wantSize: uploaded,
		},
		{
			name: "success for an upload Core never completed is a failure", report: "success",
			run:       models.BackupRun{Status: "running", UploadID: "u"},
			wantApply: true, wantStatus: "failed", wantSize: 0, wantMessage: "never completed",
		},
		{
			name: "failure after Core completed the upload counts nothing", report: "failed",
			run:       models.BackupRun{Status: "running", UploadID: "u", UploadedBytes: &uploaded},
			wantApply: true, wantStatus: "failed", wantSize: 0,
		},
		{
			name: "a status that is neither is a failure", report: "done",
			run:       models.BackupRun{Status: "running", UploadID: "u", UploadedBytes: &uploaded},
			wantApply: true, wantStatus: "failed", wantSize: 0,
		},
		{
			name: "a run Core already closed is left alone", report: "success",
			run:       models.BackupRun{Status: "failed", UploadID: "u", UploadedBytes: &uploaded},
			wantApply: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := tt.run
			apply, status, message, size := settleObjectStorageReport(tt.report, "", &run)
			if apply != tt.wantApply {
				t.Fatalf("apply = %v, want %v", apply, tt.wantApply)
			}
			if !apply {
				return
			}
			if status != tt.wantStatus || size != tt.wantSize {
				t.Errorf("status %q size %d, want %q size %d", status, size, tt.wantStatus, tt.wantSize)
			}
			if !strings.Contains(message, tt.wantMessage) {
				t.Errorf("message %q, want it to contain %q", message, tt.wantMessage)
			}
		})
	}
}

// publishUntil publishes a result on the hosting node's channel until done
// reports true, because PSUBSCRIBE is asynchronous.
func publishUntil(t *testing.T, rdb *redis.Client, payload map[string]interface{}, done func() bool) {
	t.Helper()
	data, _ := json.Marshal(payload)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		rdb.Publish(context.Background(), queue.BackupResultsChannel("node-hosting"), data)
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the result was never applied")
}

func runConsumer(t *testing.T, st *transferFakeStore, prov *fakeMultipartStorage) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	b := transferScheduler(st, prov, nil)
	b.redis = rdb
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.consumeResults(ctx)
	return rdb
}

// What a success goes on to touch: no install records, nothing to prune.
func (f *transferFakeStore) ListSubServerInstalls(int) ([]models.SubServerInstall, error) {
	return nil, nil
}
func (f *transferFakeStore) PruneOldBackupRuns(int, int) ([]models.BackupRun, error) { return nil, nil }

func (f *transferFakeStore) updateList() []reapUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]reapUpdate(nil), f.updates...)
}

// H1: through the real subscription, a success report of one byte for an
// upload Core measured at 300 GiB writes 300 GiB.
func TestConsumeResults_ObjectStorageRunTakesCoresSize(t *testing.T) {
	st := newTransferFakeStore()
	uploaded := int64(300 << 30)
	st.runs[1].UploadID, st.runs[1].PartSize, st.runs[1].UploadedBytes = "upload-done", backupPartSize, &uploaded
	st.jobs[10].RetentionCount = 3
	rdb := runConsumer(t, st, &fakeMultipartStorage{})

	publishUntil(t, rdb, map[string]interface{}{"runId": 1, "status": "success", "sizeBytes": 1},
		func() bool { return len(st.updateList()) > 0 })

	got := st.updateList()[0]
	if got.status != "success" || got.size != uploaded {
		t.Fatalf("update = %+v, want success with Core's %d bytes", got, uploaded)
	}
}

// M3: a node's first part-URL request lands between the consumer's read of the
// run and its write of "failed". The upload it stored is not in the consumer's
// copy, and must still be aborted and its key deleted.
func TestConsumeResults_DiscardsAnUploadStoredDuringTheFailedWrite(t *testing.T) {
	st := newTransferFakeStore()
	st.beforeUpdate = func(r *models.BackupRun) {
		if r.UploadID == "" {
			r.UploadID, r.PartSize = "upload-raced", backupPartSize
		}
	}
	prov := &fakeMultipartStorage{}
	rdb := runConsumer(t, st, prov)

	publishUntil(t, rdb, map[string]interface{}{"runId": 1, "status": "failed", "error": "archive failed"},
		func() bool { return len(prov.deletes()) > 0 })

	if got := st.aborts(prov); len(got) != 1 || got[0] != "upload-raced" {
		t.Fatalf("aborted = %v, want the upload stored during the write", got)
	}
	if got := prov.deletes(); got[0] != st.runs[1].StorageKey {
		t.Fatalf("deleted = %v, want the run's own key", got)
	}
}
