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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

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

	// Bucket setup with the SDK, the one thing the node never does itself.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin", "")))
	if err != nil {
		t.Fatal(err)
	}
	admin := s3.NewFromConfig(awsCfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(endpoint); o.UsePathStyle = true })
	if _, err := admin.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		if !errors.As(err, &owned) {
			t.Fatalf("create bucket: %v", err)
		}
	}

	cfg, _ := json.Marshal(map[string]interface{}{
		"endpoint": endpoint, "bucket": bucket, "region": "us-east-1", "forcePathStyle": true,
		"accessKeyId": "minioadmin", "secretAccessKey": "minioadmin", "prefix": "tenant-prefix",
	})
	storageRow := &models.BackupStorage{ID: 3, Name: "minio", Provider: "s3", Config: cfg}
	key := "backups/srv/job-1/" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".tar.gz"
	st := &e2eStore{run: models.BackupRun{ID: 1, JobID: 10, Status: "running", StorageKey: key}, storage: storageRow}
	core := services.NewBackupTransfer(st, backupstorage.Deps{})
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
