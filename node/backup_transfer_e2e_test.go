//go:build e2e

package main

// End to end for document F: Core's real node-request handlers, in process,
// against a real S3-compatible bucket, and this node's real uploader and
// restore download against the URLs those handlers sign.
//
// Behind the e2e build tag because it imports dylaris-core, which this module
// does not require: it resolves only through the platform go.work, so a plain
// `go test ./...` or a module-mode build must never compile it. Skipped unless
// DYLARIS_TEST_S3_ENDPOINT is set as well.
//
//	docker run -d --name dyl-minio -p 59002:9000 -e MINIO_ROOT_USER=minioadmin \
//	  -e MINIO_ROOT_PASSWORD=minioadmin minio/minio server /data
//	DYLARIS_TEST_S3_ENDPOINT=http://127.0.0.1:59002 go test -tags e2e -run E2E -v .

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	"dylaris-core/services"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"

	pb "dylaris-proto/node"
)

type e2eStore struct {
	store.Store
	run     models.BackupRun
	storage *models.BackupStorage
}

func (s *e2eStore) GetBackupRun(int) (*models.BackupRun, error) { r := s.run; return &r, nil }
func (s *e2eStore) GetBackupJob(id int) (*models.BackupJob, error) {
	return &models.BackupJob{ID: id, ServerID: 100}, nil
}
func (s *e2eStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, NodeID: 5, OwnerID: "alice"}, nil
}
func (s *e2eStore) GetBackupRestore(id int) (*models.BackupRestore, error) {
	return &models.BackupRestore{ID: id, RunID: 1, ServerID: 100, Status: "queued"}, nil
}
func (s *e2eStore) GetBackupStorage(int) (*models.BackupStorage, error)     { return s.storage, nil }
func (s *e2eStore) GetDefaultBackupStorage() (*models.BackupStorage, error) { return s.storage, nil }
func (s *e2eStore) GetUserDefaultBackupStorage(string) (*models.BackupStorage, error) {
	return nil, nil
}
func (s *e2eStore) SetBackupRunUpload(_ int, id string, size int64) (bool, error) {
	if s.run.UploadID != "" {
		return false, nil
	}
	s.run.UploadID, s.run.PartSize = id, size
	return true, nil
}
func (s *e2eStore) SetBackupRunUploaded(_ int, size int64) (bool, error) {
	s.run.UploadedBytes = &size
	return true, nil
}
func (s *e2eStore) TouchBackupRunTransfer(int) error { return nil }

// No allowance is configured, so the upload is not capped.
func (s *e2eStore) GetUserByID(string) (*models.User, error)          { return nil, errors.New("no user") }
func (s *e2eStore) GetUserBilling(string) (*store.UserBilling, error) { return nil, nil }
func (s *e2eStore) GetSetting(string) (string, error)                 { return "", nil }
func (s *e2eStore) BackupBytesByOwner(string) (int64, error)          { return 0, nil }

// patternReader is a deterministic, incompressible byte source.
func patternReader(n int64) io.Reader {
	return io.LimitReader(rand.New(rand.NewSource(42)), n)
}

func TestE2EMultipartBackupAndRestoreThroughCoreHandlers(t *testing.T) {
	endpoint := os.Getenv("DYLARIS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("DYLARIS_TEST_S3_ENDPOINT not set")
	}
	ctx := context.Background()
	bucket := "dylaris-f3-e2e"

	createTestBucket(t, endpoint, bucket)

	cfg, _ := json.Marshal(map[string]interface{}{
		"endpoint": endpoint, "bucket": bucket, "region": "us-east-1", "forcePathStyle": true,
		"accessKeyId": "minioadmin", "secretAccessKey": "minioadmin", "prefix": "tenant-prefix",
	})
	storageRow := &models.BackupStorage{ID: 3, Name: "minio", Provider: "s3", Config: cfg}
	key := "backups/srv/job-1/" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".tar.gz"
	st := &e2eStore{run: models.BackupRun{ID: 1, JobID: 10, Status: "running", StorageKey: key}, storage: storageRow}
	core := services.NewBackupTransfer(st, backupstorage.Deps{}, false)
	prov, err := backupstorage.Open(ctx, storageRow, backupstorage.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prov.Delete(context.Background(), key) })

	// The node's control stream, answered in process by Core's handlers with the
	// identity the stream would have authenticated.
	node := nodegrpc.Node{ID: 5, Token: "node-e2e"}
	prevReq := coreRequest
	t.Cleanup(func() { coreRequest = prevReq })
	coreRequest = func(ctx context.Context, req *pb.NodeMessage, _ time.Duration) (*pb.NodeMessage, error) {
		switch {
		case req.GetUploadPartUrlsRequest() != nil:
			return core.HandleUploadPartURLs(ctx, node, req), nil
		case req.GetCompleteUploadRequest() != nil:
			return core.HandleCompleteUpload(ctx, node, req), nil
		case req.GetRestoreUrlRequest() != nil:
			return core.HandleRestoreURL(ctx, node, req), nil
		}
		return nil, errors.New("unexpected request")
	}

	// Core's real part size is 64 MiB and not injectable, so the archive is two
	// full parts and a short third: 128 MiB + 3 MiB + 7 bytes.
	const size = 2*(64<<20) + 3<<20 + 7
	want := sha256.New()
	src := io.TeeReader(patternReader(size), want)

	got, err := uploadMultipart(ctx, coreMultipartAPI(1), objectTransferClient, src, nil)
	if err != nil {
		t.Fatalf("uploadMultipart: %v", err)
	}
	if got != size {
		t.Fatalf("Core reported %d bytes, want %d", got, size)
	}
	if st.run.PartSize != 64<<20 || st.run.UploadID == "" {
		t.Fatalf("stored upload %q part size %d", st.run.UploadID, st.run.PartSize)
	}
	if st.run.UploadedBytes == nil || *st.run.UploadedBytes != size {
		t.Fatalf("recorded size %v, want Core's measured %d", st.run.UploadedBytes, size)
	}

	obj, err := prov.Stat(ctx, key)
	if err != nil || obj.Size != size {
		t.Fatalf("Stat = %+v, %v; want %d bytes", obj, err, size)
	}
	// Completing again, as a retry after a lost reply would, answers success.
	again := core.HandleCompleteUpload(ctx, node, &pb.NodeMessage{Payload: &pb.NodeMessage_CompleteUploadRequest{
		CompleteUploadRequest: &pb.CompleteUploadRequest{RunId: "1"}}}).GetCompleteUploadResponse()
	if again.Error != "" || again.SizeBytes != size {
		t.Fatalf("second complete = %d / %q, want success with %d", again.SizeBytes, again.Error, size)
	}

	// The restore half: URL from Core, download by the node.
	restoreURL, err := coreRestoreURL(20)(ctx)
	if err != nil {
		t.Fatalf("restore URL: %v", err)
	}
	body, err := openPresignedRestore(ctx, objectTransferClient, restoreURL, coreRestoreURL(20))
	if err != nil {
		t.Fatalf("openPresignedRestore: %v", err)
	}
	defer body.Close()
	have := sha256.New()
	n, err := io.Copy(have, body)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if n != size || fmt.Sprintf("%x", have.Sum(nil)) != fmt.Sprintf("%x", want.Sum(nil)) {
		t.Fatalf("downloaded %d bytes with a different hash than the %d uploaded", n, size)
	}
	t.Logf("uploaded and restored %d bytes in 3 parts through Core-signed URLs, sha256 %x", size, want.Sum(nil))
}

// createTestBucket creates bucket on a MinIO at endpoint with the default
// minioadmin credentials, signed by hand with SigV4. The node module no longer
// depends on the AWS SDK, and a test is no reason to bring it back.
func createTestBucket(t *testing.T, endpoint, bucket string) {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	const region, secret, emptyHash = "us-east-1", "minioadmin", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	now := time.Now().UTC()
	amzDate, day := now.Format("20060102T150405Z"), now.Format("20060102")
	canonical := strings.Join([]string{"PUT", "/" + bucket, "", "host:" + u.Host,
		"x-amz-content-sha256:" + emptyHash, "x-amz-date:" + amzDate, "",
		"host;x-amz-content-sha256;x-amz-date", emptyHash}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(sum[:])}, "\n")
	mac := func(key []byte, data string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(data))
		return h.Sum(nil)
	}
	key := mac(mac(mac(mac([]byte("AWS4"+secret), day), region), "s3"), "aws4_request")

	req, err := http.NewRequest(http.MethodPut, endpoint+"/"+bucket, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", emptyHash)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=minioadmin/"+scope+
		", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+hex.EncodeToString(mac(key, toSign)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// 409 is BucketAlreadyOwnedByYou from an earlier run.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		t.Fatalf("create bucket: status %d: %s", resp.StatusCode, body)
	}
}
