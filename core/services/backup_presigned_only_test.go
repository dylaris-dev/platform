package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-pkg/queue"
)

// Document F, the scheduler's half: what a dispatched backup command carries on
// object storage, the version gate in front of it, and the three places an
// upload that will never complete is aborted.

func (f *transferFakeStore) GetNodeByID(id int) (*models.Node, error) {
	switch id {
	case 5:
		return &models.Node{ID: 5, Token: "node-hosting"}, nil
	case 6:
		return &models.Node{ID: 6, Token: "node-other"}, nil
	}
	return nil, errors.New("no such node")
}

func (f *transferFakeStore) ListAbandonedBackupRuns(time.Time, int) ([]models.BackupRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []models.BackupRun
	for _, r := range f.runs {
		if r.Status == "running" {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *transferFakeStore) CreateBackupRun(r *models.BackupRun) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	cp.ID = 2
	f.runs[2] = &cp
	return 2, nil
}

func (f *transferFakeStore) SetBackupJobScheduled(int, time.Time, time.Time) error { return nil }

// Not node-local mode, so the per-server node-local cap stays out of the way.
func (f *transferFakeStore) GetSetting(key string) (string, error) {
	if key == "backup.mode" {
		return "s3", nil
	}
	return "", nil
}

func (f *transferFakeStore) aborts(prov *fakeMultipartStorage) []string {
	prov.mu.Lock()
	defer prov.mu.Unlock()
	return append([]string(nil), prov.aborted...)
}

func transferScheduler(st *transferFakeStore, prov *fakeMultipartStorage, rdb *redis.Client) *BackupScheduler {
	b := &BackupScheduler{store: st, redis: rdb}
	if rdb != nil {
		b.queue = NewQueueService(rdb)
	}
	b.SetConnection(func(int, string) (backupstorage.Storage, error) { return prov, nil })
	return b
}

// dispatchedCommands returns the raw JSON of every command queued for a node.
func dispatchedCommands(t *testing.T, rdb *redis.Client, token string) []string {
	t.Helper()
	msgs, err := rdb.XRange(context.Background(), "dylaris:node:"+token+":cmds", "-", "+").Result()
	if err != nil {
		t.Fatalf("read command stream: %v", err)
	}
	var out []string
	for _, m := range msgs {
		out = append(out, m.Values["data"].(string))
	}
	return out
}

// A tenant-owned connection row, so the platform allowance does not apply.
func ownedConnectionStorage() *models.BackupStorage {
	owner := "alice"
	return &models.BackupStorage{ID: 3, Name: "R2", Provider: "connection", OwnerID: &owner,
		Config: json.RawMessage(`{"connectionId":1,"prefix":"backups","accessKeyId":"AKIA_LEAK","secretAccessKey":"leak-secret"}`)}
}

// The dispatched command on object storage: "upload":"multipart", the storage
// key, and nothing a node could use without Core - no URL, no credential.
func TestDispatch_ObjectStorageCommandCarriesNoSecretAndNoURL(t *testing.T) {
	st := newTransferFakeStore()
	st.storage = ownedConnectionStorage()
	st.servers[100].UUID = "srv-uuid"
	rdb := heartbeatRedis(t, map[string]string{"node-hosting": presignedMultipartSince})
	b := transferScheduler(st, &fakeMultipartStorage{}, rdb)

	if err := b.dispatch(context.Background(), models.BackupJob{ID: 10, ServerID: 100, Schedule: "every 1d"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	cmds := dispatchedCommands(t, rdb, "node-hosting")
	if len(cmds) != 1 {
		t.Fatalf("commands = %d, want 1", len(cmds))
	}
	raw := cmds[0]
	var cmd map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		t.Fatalf("decode command: %v", err)
	}
	if cmd["upload"] != "multipart" {
		t.Errorf("upload = %v, want multipart", cmd["upload"])
	}
	if key, _ := cmd["storageKey"].(string); !strings.HasPrefix(key, "backups/srv-uuid/job-10/") {
		t.Errorf("storageKey = %q", key)
	}
	for _, forbidden := range []string{"presignedPutUrl", "presignedGetUrl", "X-Amz-", "http://", "https://", "AKIA_LEAK", "leak-secret", "accessKeyId", "secretAccessKey"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("command contains %q: %s", forbidden, raw)
		}
	}
	if storage, _ := cmd["storage"].(map[string]interface{}); storage == nil || len(storage["config"].(map[string]interface{})) != 0 {
		t.Errorf("storage = %v, want the row with an empty config", cmd["storage"])
	}
}

// An old or unknown node is not sent the command at all, and the run says why.
func TestDispatch_RefusesANodeTooOldAndFailsTheRun(t *testing.T) {
	for name, versions := range map[string]map[string]string{
		"older release": {"node-hosting": "2026.09.14"},
		"garbage":       {"node-hosting": "dev"},
		"no heartbeat":  {},
	} {
		t.Run(name, func(t *testing.T) {
			st := newTransferFakeStore()
			st.storage = ownedConnectionStorage()
			rdb := heartbeatRedis(t, versions)
			b := transferScheduler(st, &fakeMultipartStorage{}, rdb)

			err := b.dispatch(context.Background(), models.BackupJob{ID: 10, ServerID: 100, Schedule: "every 1d"})
			if !errors.Is(err, ErrNodeUpdateRequired) {
				t.Fatalf("err = %v, want ErrNodeUpdateRequired", err)
			}
			if cmds := dispatchedCommands(t, rdb, "node-hosting"); len(cmds) != 0 {
				t.Fatalf("a command reached the node: %v", cmds)
			}
			if len(st.updates) != 1 || st.updates[0].id != 2 || st.updates[0].status != "failed" ||
				!strings.Contains(st.updates[0].message, "must be updated") {
				t.Fatalf("updates = %+v, want run 2 failed with the update message", st.updates)
			}
		})
	}
}

// A failed report for a run with an upload aborts that upload, through the real
// subscription the node publishes on.
func TestConsumeResults_AbortsTheUploadOfAFailedRun(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	st := newTransferFakeStore()
	st.runs[1].UploadID, st.runs[1].PartSize = "upload-live", backupPartSize
	prov := &fakeMultipartStorage{}
	b := transferScheduler(st, prov, nil)
	b.redis = rdb

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.consumeResults(ctx)

	data, _ := json.Marshal(map[string]interface{}{"runId": 1, "status": "failed", "error": "upload failed: put status 500"})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(st.aborts(prov)) == 0 {
		rdb.Publish(ctx, queue.BackupResultsChannel("node-hosting"), data)
		time.Sleep(20 * time.Millisecond)
	}
	if got := st.aborts(prov); len(got) == 0 || got[0] != "upload-live" {
		t.Fatalf("aborted = %v, want upload-live", got)
	}
}

// A run that succeeded stays succeeded: the failure a redelivered command
// reports afterwards must neither overwrite it nor abort anything.
func TestApplyBackupReport_ASucceededRunStaysSucceeded(t *testing.T) {
	for _, report := range []string{"failed", "running", "success"} {
		if apply, _ := applyBackupReport(report, "success", time.Now()); apply {
			t.Errorf("a %q report was applied to a succeeded run", report)
		}
	}
}

func TestReapAbandonedRuns_AbortsTheUpload(t *testing.T) {
	st := newTransferFakeStore()
	st.runs[1].StartedAt = time.Now().Add(-8 * time.Hour)
	st.runs[1].UploadID, st.runs[1].PartSize = "upload-stale", backupPartSize
	prov := &fakeMultipartStorage{}

	transferScheduler(st, prov, nil).reapAbandonedRuns(context.Background(), time.Now())

	if len(st.updates) != 1 || st.updates[0].status != "failed" {
		t.Fatalf("updates = %+v, want the run closed as failed", st.updates)
	}
	if got := st.aborts(prov); len(got) != 1 || got[0] != "upload-stale" {
		t.Fatalf("aborted = %v, want upload-stale", got)
	}
}

func TestReapAbandonedRuns_NoUploadNoAbort(t *testing.T) {
	st := newTransferFakeStore()
	st.runs[1].StartedAt = time.Now().Add(-8 * time.Hour)
	prov := &fakeMultipartStorage{}

	transferScheduler(st, prov, nil).reapAbandonedRuns(context.Background(), time.Now())

	if got := st.aborts(prov); len(got) != 0 {
		t.Fatalf("aborted = %v for a run that never started an upload", got)
	}
}
