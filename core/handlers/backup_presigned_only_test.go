package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Document F, the request handlers' half: a manual backup and a restore on
// object storage send the node no URL and no credential, an old node is refused
// before it is told anything, and deleting a run that is still uploading aborts
// its upload.

type presignedOnlyStore struct {
	store.Store

	storage *models.BackupStorage
	run     models.BackupRun

	mu             sync.Mutex
	runUpdates     []string // "<id>:<status>:<message>"
	restoreUpdates []string
	deleted        []int
}

func (f *presignedOnlyStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-uuid", NodeID: 5, OwnerID: "alice"}, nil
}
func (f *presignedOnlyStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id, Token: "node-hosting"}, nil
}
func (f *presignedOnlyStore) GetBackupJob(id int) (*models.BackupJob, error) {
	return &models.BackupJob{ID: id, ServerID: 100, Schedule: "manual"}, nil
}
func (f *presignedOnlyStore) GetBackupRun(int) (*models.BackupRun, error) {
	r := f.run
	return &r, nil
}
func (f *presignedOnlyStore) GetBackupStorage(int) (*models.BackupStorage, error) {
	return f.storage, nil
}
func (f *presignedOnlyStore) GetDefaultBackupStorage() (*models.BackupStorage, error) {
	return f.storage, nil
}
func (f *presignedOnlyStore) GetUserDefaultBackupStorage(string) (*models.BackupStorage, error) {
	return nil, nil
}
func (f *presignedOnlyStore) GetSetting(key string) (string, error) {
	if key == "backup.mode" {
		return "s3", nil
	}
	return "", nil
}
func (f *presignedOnlyStore) GetUserByUsername(string) (*models.User, error) { return nil, nil }
func (f *presignedOnlyStore) CreateBackupRun(*models.BackupRun) (int, error) { return 2, nil }
func (f *presignedOnlyStore) UpdateBackupRunStatus(id int, status, msg string, _ int64, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runUpdates = append(f.runUpdates, strings.Join([]string{strconv.Itoa(id), status, msg}, ":"))
	return nil
}
func (f *presignedOnlyStore) CreateBackupRestore(*models.BackupRestore) (int, error) { return 30, nil }
func (f *presignedOnlyStore) UpdateBackupRestoreStatus(id int, status, msg string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restoreUpdates = append(f.restoreUpdates, strings.Join([]string{strconv.Itoa(id), status, msg}, ":"))
	return nil
}
func (f *presignedOnlyStore) DeleteBackupRun(id int) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *presignedOnlyStore) SetBackupRunManifest(int, string) error { return nil }
func (f *presignedOnlyStore) GetSubServerInstall(int, string) (*models.SubServerInstall, error) {
	return nil, nil
}
func (f *presignedOnlyStore) ListSubServerInstalls(int) ([]models.SubServerInstall, error) {
	return nil, nil
}
func (f *presignedOnlyStore) ListServerMods(int, string) ([]models.ServerMod, error) { return nil, nil }
func (f *presignedOnlyStore) ListServerModSubServers(int) ([]string, error)          { return nil, nil }

// s3Recorder is an S3 endpoint that answers every request with 204 and records
// method, path and query.
type s3Recorder struct {
	mu   sync.Mutex
	seen []string
}

func (s *s3Recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.seen = append(s.seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *s3Recorder) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func presignedOnlyHandler(t *testing.T, nodeVersion string, run models.BackupRun) (*BackupHandler, *presignedOnlyStore, *redis.Client, *s3Recorder) {
	t.Helper()
	rec := &s3Recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	owner := "alice"
	cfg, _ := json.Marshal(map[string]interface{}{
		"endpoint": srv.URL, "bucket": "b", "region": "us-east-1", "forcePathStyle": true,
		"accessKeyId": "AKIA_LEAK", "secretAccessKey": "leak-secret",
	})
	fs := &presignedOnlyStore{
		storage: &models.BackupStorage{ID: 3, Name: "own bucket", Provider: "s3", OwnerID: &owner, Config: cfg},
		run:     run,
	}

	mr := miniredis.RunT(t)
	if nodeVersion != "-" {
		hb, _ := json.Marshal(services.NodeHeartbeat{ID: "node-hosting", ReleaseVersion: nodeVersion})
		mr.Set("dylaris:discovery:node-hosting", string(hb))
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	h := &BackupHandler{state: &AppState{Store: fs, Authz: authz.NewResolver(fs), Redis: rdb, Queue: services.NewQueueService(rdb)}}
	return h, fs, rdb, rec
}

func queuedCommands(t *testing.T, rdb *redis.Client) []string {
	t.Helper()
	msgs, err := rdb.XRange(context.Background(), "dylaris:node:node-hosting:cmds", "-", "+").Result()
	if err != nil {
		t.Fatalf("read command stream: %v", err)
	}
	var out []string
	for _, m := range msgs {
		out = append(out, m.Values["data"].(string))
	}
	return out
}

func assertNoSecretOrURL(t *testing.T, raw string) {
	t.Helper()
	for _, forbidden := range []string{"presignedPutUrl", "presignedGetUrl", "X-Amz-", "http://", "https://", "AKIA_LEAK", "leak-secret"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("command contains %q: %s", forbidden, raw)
		}
	}
}

const currentNodeVersion = "2026.09.14.2"

func TestRestoreRun_RefusesAnOldNodeBeforeTellingItAnything(t *testing.T) {
	for name, version := range map[string]string{"older": "2026.09.14", "unparseable": "dev", "no heartbeat": "-"} {
		t.Run(name, func(t *testing.T) {
			h, fs, rdb, _ := presignedOnlyHandler(t, version, models.BackupRun{ID: 1, JobID: 10, Status: "success", StorageKey: "backups/k.tar.gz"})
			rw := httptest.NewRecorder()
			h.RestoreRun(rw, backupJobRequest(http.MethodPost, "/api/backup-runs/1/restore", "", map[string]string{"runId": "1"}))

			if rw.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 (body %s)", rw.Code, rw.Body.String())
			}
			// Nothing queued: the node stops the server before it downloads, so a
			// command it cannot carry out would take the server down for nothing.
			if cmds := queuedCommands(t, rdb); len(cmds) != 0 {
				t.Fatalf("a restore reached the node: %v", cmds)
			}
			if len(fs.restoreUpdates) != 1 || !strings.HasPrefix(fs.restoreUpdates[0], "30:failed:") || !strings.Contains(fs.restoreUpdates[0], "must be updated") {
				t.Fatalf("restore updates = %v, want restore 30 failed with the update message", fs.restoreUpdates)
			}
		})
	}
}

func TestRestoreRun_ObjectStorageCommandCarriesNoURL(t *testing.T) {
	h, _, rdb, rec := presignedOnlyHandler(t, currentNodeVersion, models.BackupRun{ID: 1, JobID: 10, Status: "success", StorageKey: "backups/k.tar.gz"})
	rw := httptest.NewRecorder()
	h.RestoreRun(rw, backupJobRequest(http.MethodPost, "/api/backup-runs/1/restore", "", map[string]string{"runId": "1"}))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rw.Code, rw.Body.String())
	}
	cmds := queuedCommands(t, rdb)
	if len(cmds) != 1 {
		t.Fatalf("commands = %d, want 1", len(cmds))
	}
	var cmd map[string]interface{}
	if err := json.Unmarshal([]byte(cmds[0]), &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd["download"] != "presigned" || cmd["restoreId"] != float64(30) {
		t.Errorf("download = %v restoreId = %v, want presigned / 30", cmd["download"], cmd["restoreId"])
	}
	assertNoSecretOrURL(t, cmds[0])
	if got := rec.requests(); len(got) != 0 {
		t.Errorf("dispatch reached the bucket: %v", got)
	}
}

func TestTriggerJob_ObjectStorage(t *testing.T) {
	t.Run("an old node is refused and the run failed", func(t *testing.T) {
		h, fs, rdb, _ := presignedOnlyHandler(t, "2026.09.13", models.BackupRun{})
		rw := httptest.NewRecorder()
		h.TriggerJob(rw, backupJobRequest(http.MethodPost, "/api/backup-jobs/10/trigger", "", map[string]string{"jobId": "10"}))

		if rw.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %s)", rw.Code, rw.Body.String())
		}
		if cmds := queuedCommands(t, rdb); len(cmds) != 0 {
			t.Fatalf("a backup reached the node: %v", cmds)
		}
		if len(fs.runUpdates) != 1 || !strings.HasPrefix(fs.runUpdates[0], "2:failed:") || !strings.Contains(fs.runUpdates[0], "must be updated") {
			t.Fatalf("run updates = %v", fs.runUpdates)
		}
	})
	t.Run("a current node gets a multipart command without secrets", func(t *testing.T) {
		h, _, rdb, _ := presignedOnlyHandler(t, currentNodeVersion, models.BackupRun{})
		rw := httptest.NewRecorder()
		h.TriggerJob(rw, backupJobRequest(http.MethodPost, "/api/backup-jobs/10/trigger", "", map[string]string{"jobId": "10"}))

		if rw.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", rw.Code, rw.Body.String())
		}
		cmds := queuedCommands(t, rdb)
		if len(cmds) != 1 {
			t.Fatalf("commands = %d, want 1", len(cmds))
		}
		var cmd map[string]interface{}
		if err := json.Unmarshal([]byte(cmds[0]), &cmd); err != nil {
			t.Fatal(err)
		}
		if cmd["upload"] != "multipart" {
			t.Errorf("upload = %v, want multipart", cmd["upload"])
		}
		assertNoSecretOrURL(t, cmds[0])
	})
}

// Deleting a run that is still uploading aborts its upload before the row goes;
// a finished run's upload is long gone and is not asked about.
func TestDeleteRun_AbortsTheUploadOfARunningRun(t *testing.T) {
	for _, tc := range []struct {
		status    string
		wantAbort bool
	}{
		{"running", true},
		{"success", false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			h, fs, _, rec := presignedOnlyHandler(t, currentNodeVersion, models.BackupRun{
				ID: 1, JobID: 10, Status: tc.status, StorageKey: "backups/k.tar.gz", UploadID: "upload-open", PartSize: 64 << 20,
			})
			rw := httptest.NewRecorder()
			h.DeleteRun(rw, backupJobRequest(http.MethodDelete, "/api/backup-runs/1", "", map[string]string{"runId": "1"}))

			if rw.Code != http.StatusOK {
				t.Fatalf("status = %d (body %s)", rw.Code, rw.Body.String())
			}
			aborted, objectDeleted := false, false
			for _, r := range rec.requests() {
				if strings.HasPrefix(r, "DELETE ") && strings.Contains(r, "uploadId=upload-open") {
					aborted = true
				}
				// The archive a completed upload left behind goes with the run.
				if strings.HasPrefix(r, "DELETE ") && strings.Contains(r, "backups/k.tar.gz") && !strings.Contains(r, "uploadId=") {
					objectDeleted = true
				}
			}
			if aborted != tc.wantAbort {
				t.Errorf("abort sent = %v, want %v (requests %v)", aborted, tc.wantAbort, rec.requests())
			}
			if !objectDeleted {
				t.Errorf("the run's object was not deleted (requests %v)", rec.requests())
			}
			if len(fs.deleted) != 1 {
				t.Errorf("row deleted %d times, want 1", len(fs.deleted))
			}
		})
	}
}
