package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"

	pb "dylaris-proto/node"
)

// Document F: a node asks Core for part URLs, for the completion of its upload
// and for a restore URL, on its authenticated control stream. These pin who may
// ask, what happens when two replicas answer the same request, and that a
// retried completion is not mistaken for a lost upload.

// transferFakeStore answers the run -> job -> server chain and the restore row,
// and implements SetBackupRunUpload with the same condition as the SQL.
type transferFakeStore struct {
	store.Store

	mu       sync.Mutex
	runs     map[int]*models.BackupRun
	jobs     map[int]*models.BackupJob
	servers  map[int]*models.Server
	restores map[int]*models.BackupRestore
	storage  *models.BackupStorage
	updates  []reapUpdate
	// setUploadHook runs inside SetBackupRunUpload before the condition is
	// evaluated, for tests that need to line up concurrent callers.
	setUploadHook func()
	// beforeUpdate runs inside UpdateBackupRunStatus, under the lock, before the
	// status is written: what another request did between a caller's read and
	// its write.
	beforeUpdate func(r *models.BackupRun)
	// quotaGB is the operator allowance (SettingBackupDefaultUserQuota), "" for
	// none; usedBytes is what the owner already stores.
	quotaGB   string
	usedBytes int64
	touches   int
}

func newTransferFakeStore() *transferFakeStore {
	return &transferFakeStore{
		runs:     map[int]*models.BackupRun{1: {ID: 1, JobID: 10, Status: "running", StorageKey: "backups/srv/job-10/run.tar.gz"}},
		jobs:     map[int]*models.BackupJob{10: {ID: 10, ServerID: 100}},
		servers:  map[int]*models.Server{100: {ID: 100, NodeID: 5, OwnerID: "alice"}},
		restores: map[int]*models.BackupRestore{20: {ID: 20, RunID: 1, ServerID: 100, Status: "queued"}},
		storage: &models.BackupStorage{ID: 3, Name: "R2", Provider: "connection",
			Config: json.RawMessage(`{"connectionId":1,"prefix":"backups"}`)},
	}
}

func (f *transferFakeStore) GetBackupRun(id int) (*models.BackupRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, errors.New("no such run")
	}
	cp := *r
	return &cp, nil
}

func (f *transferFakeStore) GetBackupJob(id int) (*models.BackupJob, error) {
	j, ok := f.jobs[id]
	if !ok {
		return nil, errors.New("no such job")
	}
	return j, nil
}

func (f *transferFakeStore) GetServerByID(id int) (*models.Server, error) {
	s, ok := f.servers[id]
	if !ok {
		return nil, errors.New("no such server")
	}
	return s, nil
}

func (f *transferFakeStore) GetBackupRestore(id int) (*models.BackupRestore, error) {
	r, ok := f.restores[id]
	if !ok {
		return nil, errors.New("no such restore")
	}
	return r, nil
}

func (f *transferFakeStore) GetBackupStorage(int) (*models.BackupStorage, error) {
	return f.storage, nil
}
func (f *transferFakeStore) GetDefaultBackupStorage() (*models.BackupStorage, error) {
	return f.storage, nil
}
func (f *transferFakeStore) GetUserDefaultBackupStorage(string) (*models.BackupStorage, error) {
	return nil, nil
}

func (f *transferFakeStore) SetBackupRunUpload(runID int, uploadID string, partSize int64) (bool, error) {
	if f.setUploadHook != nil {
		f.setUploadHook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[runID]
	if !ok || r.UploadID != "" || r.Status != "running" {
		return false, nil
	}
	r.UploadID, r.PartSize = uploadID, partSize
	return true, nil
}

func (f *transferFakeStore) UpdateBackupRunStatus(id int, status, message string, size int64, key string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, reapUpdate{id: id, status: status, message: message, size: size, key: key})
	if r, ok := f.runs[id]; ok {
		if f.beforeUpdate != nil {
			f.beforeUpdate(r)
		}
		r.Status = status
	}
	return nil
}

// SetBackupRunUploaded and TouchBackupRunTransfer carry the SQL's condition.
func (f *transferFakeStore) SetBackupRunUploaded(runID int, size int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[runID]
	if !ok || r.Status != "running" {
		return false, nil
	}
	r.UploadedBytes = &size
	return true, nil
}

func (f *transferFakeStore) TouchBackupRunTransfer(int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches++
	return nil
}

func (f *transferFakeStore) GetUserByID(string) (*models.User, error) {
	return nil, errors.New("no such user")
}
func (f *transferFakeStore) GetUserBilling(string) (*store.UserBilling, error) { return nil, nil }
func (f *transferFakeStore) BackupBytesByOwner(string) (int64, error)          { return f.usedBytes, nil }

// fakeMultipartStorage records the multipart calls. URLs encode what was signed,
// so a test can read the upload id, part number and TTL back out of them.
type fakeMultipartStorage struct {
	backupstorage.Storage

	mu          sync.Mutex
	creates     int
	aborted     []string
	completes   int
	completeErr error
	objects     map[string]int64
	deleted     []string
	// usage is what ListMultipart reports; listErr fails it.
	usage   backupstorage.MultipartUsage
	listErr error
	// createHook runs after an upload id is minted, outside the lock.
	createHook func()
	// completeHook runs as a completion starts, outside the lock; completeSize,
	// when set, is the size the completion reports.
	completeHook func()
	completeSize int64
}

func (s *fakeMultipartStorage) CreateMultipart(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	s.creates++
	id := fmt.Sprintf("upload-%d", s.creates)
	s.mu.Unlock()
	if s.createHook != nil {
		s.createHook()
	}
	return id, nil
}

func (s *fakeMultipartStorage) UploadPartURL(_ context.Context, key, uploadID string, part int32, ttl time.Duration) (string, error) {
	return fmt.Sprintf("https://bucket.test/%s?uploadId=%s&partNumber=%d&ttl=%s", key, uploadID, part, ttl), nil
}

func (s *fakeMultipartStorage) CompleteMultipart(_ context.Context, key, uploadID string, partSize int64) (int64, error) {
	if s.completeHook != nil {
		s.completeHook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completes++
	if s.completeErr != nil {
		return 0, s.completeErr
	}
	if s.objects == nil {
		s.objects = map[string]int64{}
	}
	s.objects[key] = 3*partSize + 17
	if s.completeSize > 0 {
		s.objects[key] = s.completeSize
	}
	return s.objects[key], nil
}

func (s *fakeMultipartStorage) AbortMultipart(_ context.Context, key, uploadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborted = append(s.aborted, uploadID)
	return nil
}

func (s *fakeMultipartStorage) ListMultipart(context.Context, string, string) (backupstorage.MultipartUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage, s.listErr
}

func (s *fakeMultipartStorage) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, key)
	delete(s.objects, key)
	return nil
}

func (s *fakeMultipartStorage) deletes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

func (s *fakeMultipartStorage) Stat(_ context.Context, key string) (backupstorage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if size, ok := s.objects[key]; ok {
		return backupstorage.Object{Key: key, Size: size}, nil
	}
	return backupstorage.Object{}, fs.ErrNotExist
}

func (s *fakeMultipartStorage) DownloadURL(_ context.Context, key string, ttl time.Duration) (string, error) {
	return fmt.Sprintf("https://bucket.test/%s?get=1&ttl=%s", key, ttl), nil
}

func newTestTransfer(st *transferFakeStore, prov *fakeMultipartStorage) *BackupTransfer {
	return NewBackupTransfer(st, backupstorage.Deps{
		Connection: func(int, string) (backupstorage.Storage, error) { return prov, nil },
	}, false)
}

var hostingNode = nodegrpc.Node{ID: 5, Token: "node-hosting"}
var otherNode = nodegrpc.Node{ID: 6, Token: "node-other"}

func partURLsMsg(runID string, parts ...int32) *pb.NodeMessage {
	return &pb.NodeMessage{Payload: &pb.NodeMessage_UploadPartUrlsRequest{
		UploadPartUrlsRequest: &pb.UploadPartUrlsRequest{RunId: runID, PartNumbers: parts}}}
}

func completeMsg(runID string) *pb.NodeMessage {
	return &pb.NodeMessage{Payload: &pb.NodeMessage_CompleteUploadRequest{
		CompleteUploadRequest: &pb.CompleteUploadRequest{RunId: runID}}}
}

func restoreURLMsg(restoreID string) *pb.NodeMessage {
	return &pb.NodeMessage{Payload: &pb.NodeMessage_RestoreUrlRequest{
		RestoreUrlRequest: &pb.RestoreUrlRequest{RestoreId: restoreID}}}
}

// transferError returns the error field of whichever typed response came back,
// and fails the test when the answer is not a typed response at all.
func transferError(t *testing.T, resp *pb.NodeMessage) string {
	t.Helper()
	switch {
	case resp.GetUploadPartUrlsResponse() != nil:
		return resp.GetUploadPartUrlsResponse().Error
	case resp.GetCompleteUploadResponse() != nil:
		return resp.GetCompleteUploadResponse().Error
	case resp.GetRestoreUrlResponse() != nil:
		return resp.GetRestoreUrlResponse().Error
	}
	t.Fatalf("not a typed transfer response: %v", resp)
	return ""
}

// Only the node hosting the run's server may touch its upload or its restore.
// The refusal travels in the typed response - an OpError would make the node
// retry on another replica, which would refuse again.
func TestBackupTransfer_RefusesAnotherNode(t *testing.T) {
	st := newTransferFakeStore()
	st.runs[1].UploadID, st.runs[1].PartSize = "upload-existing", backupPartSize
	prov := &fakeMultipartStorage{objects: map[string]int64{}}
	tr := newTestTransfer(st, prov)
	ctx := context.Background()

	for name, call := range map[string]func(nodegrpc.Node) *pb.NodeMessage{
		"part URLs": func(n nodegrpc.Node) *pb.NodeMessage { return tr.HandleUploadPartURLs(ctx, n, partURLsMsg("1", 1)) },
		"complete":  func(n nodegrpc.Node) *pb.NodeMessage { return tr.HandleCompleteUpload(ctx, n, completeMsg("1")) },
		"restore":   func(n nodegrpc.Node) *pb.NodeMessage { return tr.HandleRestoreURL(ctx, n, restoreURLMsg("20")) },
	} {
		t.Run(name, func(t *testing.T) {
			resp := call(otherNode)
			if msg := transferError(t, resp); msg == "" {
				t.Fatalf("a foreign node was served: %v", resp)
			}
			if u := resp.GetUploadPartUrlsResponse(); u != nil && (len(u.Urls) > 0 || u.UploadId != "") {
				t.Errorf("a refusal carried URLs or an upload id: %v", u)
			}
			if r := resp.GetRestoreUrlResponse(); r != nil && r.Url != "" {
				t.Errorf("a refusal carried a URL: %v", r)
			}
			// The control: the same request from the hosting node is served.
			if msg := transferError(t, call(hostingNode)); msg != "" {
				t.Fatalf("the hosting node was refused: %s", msg)
			}
		})
	}
	if prov.completes != 1 {
		t.Errorf("completes = %d, want 1 (the hosting node's only)", prov.completes)
	}
}

// A closed run gets no URLs and no completion; a finished restore no URL.
func TestBackupTransfer_RefusesWhatIsNoLongerRunning(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"failed", "success"} {
		t.Run(status, func(t *testing.T) {
			st := newTransferFakeStore()
			st.runs[1].Status = status
			st.runs[1].UploadID, st.runs[1].PartSize = "upload-existing", backupPartSize
			st.restores[20].Status = status
			prov := &fakeMultipartStorage{}
			tr := newTestTransfer(st, prov)

			if msg := transferError(t, tr.HandleUploadPartURLs(ctx, hostingNode, partURLsMsg("1", 1))); !strings.Contains(msg, "no longer running") {
				t.Errorf("part URLs: error = %q, want a no-longer-running refusal", msg)
			}
			if msg := transferError(t, tr.HandleCompleteUpload(ctx, hostingNode, completeMsg("1"))); !strings.Contains(msg, "no longer running") {
				t.Errorf("complete: error = %q, want a no-longer-running refusal", msg)
			}
			if msg := transferError(t, tr.HandleRestoreURL(ctx, hostingNode, restoreURLMsg("20"))); !strings.Contains(msg, "no longer running") {
				t.Errorf("restore: error = %q, want a no-longer-running refusal", msg)
			}
			if prov.creates != 0 || prov.completes != 0 {
				t.Errorf("storage was touched: creates=%d completes=%d", prov.creates, prov.completes)
			}
		})
	}
}

func TestBackupTransfer_RefusesBadPartRequests(t *testing.T) {
	ctx := context.Background()
	for name, msg := range map[string]*pb.NodeMessage{
		"no parts":            partURLsMsg("1"),
		"part zero":           partURLsMsg("1", 0),
		"part past the last":  partURLsMsg("1", 10001),
		"too many at once":    partURLsMsg("1", 1, 2, 3, 4, 5, 6, 7, 8, 9),
		"run id not a number": partURLsMsg("one", 1),
		"no payload":          {},
	} {
		t.Run(name, func(t *testing.T) {
			prov := &fakeMultipartStorage{}
			resp := newTestTransfer(newTransferFakeStore(), prov).HandleUploadPartURLs(ctx, hostingNode, msg)
			if transferError(t, resp) == "" {
				t.Fatalf("served: %v", resp)
			}
			if prov.creates != 0 {
				t.Errorf("an upload was created for a refused request")
			}
		})
	}
}

// The first request starts the upload and signs part URLs with the short TTL;
// a second request, as the node's retry on another replica sends, reuses it.
func TestBackupTransfer_PartURLsStartTheUploadOnce(t *testing.T) {
	st := newTransferFakeStore()
	prov := &fakeMultipartStorage{}
	tr := newTestTransfer(st, prov)
	ctx := context.Background()

	first := tr.HandleUploadPartURLs(ctx, hostingNode, partURLsMsg("1", 1, 2)).GetUploadPartUrlsResponse()
	if first.Error != "" {
		t.Fatalf("error: %s", first.Error)
	}
	if first.UploadId != "upload-1" || first.PartSize != backupPartSize {
		t.Errorf("upload %q part size %d, want upload-1 / %d", first.UploadId, first.PartSize, backupPartSize)
	}
	if len(first.Urls) != 2 || first.Urls[0].PartNumber != 1 || first.Urls[1].PartNumber != 2 {
		t.Fatalf("urls = %v, want parts 1 and 2 in order", first.Urls)
	}
	for _, u := range first.Urls {
		if !strings.Contains(u.Url, "uploadId=upload-1") || !strings.Contains(u.Url, "ttl=15m0s") {
			t.Errorf("url %q: want the stored upload and a 15 minute TTL", u.Url)
		}
	}

	again := tr.HandleUploadPartURLs(ctx, hostingNode, partURLsMsg("1", 1)).GetUploadPartUrlsResponse()
	if again.Error != "" || again.UploadId != "upload-1" {
		t.Fatalf("second request: upload %q error %q, want upload-1 reused", again.UploadId, again.Error)
	}
	if prov.creates != 1 {
		t.Errorf("creates = %d, want 1", prov.creates)
	}
	if st.runs[1].UploadID != "upload-1" || st.runs[1].PartSize != backupPartSize {
		t.Errorf("stored upload %q part size %d", st.runs[1].UploadID, st.runs[1].PartSize)
	}
}

// Two replicas answering the same first request at the same moment: both create
// an upload, exactly one is stored, the other is aborted, and both answers name
// the stored one.
func TestBackupTransfer_ConcurrentFirstRequestsKeepOneUpload(t *testing.T) {
	st := newTransferFakeStore()
	prov := &fakeMultipartStorage{}
	// Neither caller may reach the store until both have created an upload,
	// which is the interleaving that makes the conditional write matter.
	var created sync.WaitGroup
	created.Add(2)
	prov.createHook = created.Done
	st.setUploadHook = created.Wait
	tr := newTestTransfer(st, prov)

	resps := make([]*pb.UploadPartUrlsResponse, 2)
	var wg sync.WaitGroup
	for i := range resps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resps[i] = tr.HandleUploadPartURLs(context.Background(), hostingNode, partURLsMsg("1", 1)).GetUploadPartUrlsResponse()
		}(i)
	}
	wg.Wait()

	stored := st.runs[1].UploadID
	if prov.creates != 2 {
		t.Fatalf("creates = %d, want 2 (the interleaving did not happen)", prov.creates)
	}
	for i, r := range resps {
		if r.Error != "" || r.UploadId != stored {
			t.Errorf("response %d: upload %q error %q, want the stored %q", i, r.UploadId, r.Error, stored)
		}
	}
	if len(prov.aborted) != 1 || prov.aborted[0] == stored {
		t.Fatalf("aborted = %v, want exactly the one upload that was not stored (%q)", prov.aborted, stored)
	}
}

func TestBackupTransfer_CompleteReturnsTheSize(t *testing.T) {
	st := newTransferFakeStore()
	st.runs[1].UploadID, st.runs[1].PartSize = "upload-1", backupPartSize
	prov := &fakeMultipartStorage{}
	resp := newTestTransfer(st, prov).HandleCompleteUpload(context.Background(), hostingNode, completeMsg("1")).GetCompleteUploadResponse()
	if resp.Error != "" || resp.SizeBytes != 3*backupPartSize+17 {
		t.Fatalf("size %d error %q, want %d", resp.SizeBytes, resp.Error, 3*backupPartSize+17)
	}
}

// The node's retry after a lost reply meets an upload that no longer exists.
// The object is there, so the answer is success with its size, not an error that
// would fail a good backup.
func TestBackupTransfer_CompleteAfterAnEarlierCompletionSucceeds(t *testing.T) {
	st := newTransferFakeStore()
	st.runs[1].UploadID, st.runs[1].PartSize = "upload-1", backupPartSize
	prov := &fakeMultipartStorage{
		completeErr: errors.New("NoSuchUpload: the upload does not exist"),
		objects:     map[string]int64{st.runs[1].StorageKey: 987654},
	}
	resp := newTestTransfer(st, prov).HandleCompleteUpload(context.Background(), hostingNode, completeMsg("1")).GetCompleteUploadResponse()
	if resp.Error != "" || resp.SizeBytes != 987654 {
		t.Fatalf("size %d error %q, want success with the object's 987654 bytes", resp.SizeBytes, resp.Error)
	}

	// The control: no object means the failure is real, and the node is told
	// without the backend's own error text.
	prov.objects = nil
	resp = newTestTransfer(st, prov).HandleCompleteUpload(context.Background(), hostingNode, completeMsg("1")).GetCompleteUploadResponse()
	if resp.Error == "" {
		t.Fatal("a failed completion with no object was answered as success")
	}
	if strings.Contains(resp.Error, "NoSuchUpload") {
		t.Errorf("error %q passes the backend's error to the node", resp.Error)
	}
}

func TestBackupTransfer_CompleteWithoutAnUploadIsRefused(t *testing.T) {
	prov := &fakeMultipartStorage{}
	resp := newTestTransfer(newTransferFakeStore(), prov).HandleCompleteUpload(context.Background(), hostingNode, completeMsg("1")).GetCompleteUploadResponse()
	if !strings.Contains(resp.Error, "no upload was started") {
		t.Fatalf("error = %q, want a refusal naming the missing upload", resp.Error)
	}
	if prov.completes != 0 {
		t.Error("the backend was asked to complete an upload that was never started")
	}
}

func TestBackupTransfer_RestoreURLIsSignedForTheRunsKey(t *testing.T) {
	st := newTransferFakeStore()
	resp := newTestTransfer(st, &fakeMultipartStorage{}).HandleRestoreURL(context.Background(), hostingNode, restoreURLMsg("20")).GetRestoreUrlResponse()
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
	if !strings.Contains(resp.Url, st.runs[1].StorageKey) || !strings.Contains(resp.Url, "ttl=30m0s") {
		t.Errorf("url %q: want the run's key and a 30 minute TTL", resp.Url)
	}
}
