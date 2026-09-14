package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
)

// leakyErr is what a real backend error looks like: it names the endpoint, the
// bucket and a signature. None of it may reach the panel's message.
const leakyErr = "PUT https://acct.r2.cloudflarestorage.com/secret-bucket/__dylaris_probe?X-Amz-Signature=deadbeef: boom"

// multipartProbeFake records every storage call, in order, together with the
// part PUTs that reach the httptest server playing the presigned endpoint.
type multipartProbeFake struct {
	probeFakeStorage
	srv *httptest.Server

	mu    sync.Mutex
	calls []string
	parts map[string]int64

	failAt   string // create, sign, complete, stat, delete
	statSize int64  // non-zero: Stat reports this size instead of the real one
	// endpoint answers a part PUT instead of accepting it, when set. Set before
	// the probe runs; read by the server goroutine.
	endpoint atomic.Pointer[http.HandlerFunc]
}

func newMultipartProbeFake(t *testing.T) *multipartProbeFake {
	t.Helper()
	f := &multipartProbeFake{parts: map[string]int64{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		part := r.URL.Query().Get("partNumber")
		f.record(fmt.Sprintf("put %s len=%d cl=%d auth=%q", part, len(body), r.ContentLength, r.Header.Get("Authorization")))
		if h := f.endpoint.Load(); h != nil {
			(*h)(w, r)
			return
		}
		f.mu.Lock()
		f.parts[part] = int64(len(body))
		f.mu.Unlock()
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *multipartProbeFake) answerParts(status int) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
	f.endpoint.Store(&h)
}

func (f *multipartProbeFake) record(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *multipartProbeFake) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *multipartProbeFake) has(prefix string) bool {
	for _, c := range f.recorded() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *multipartProbeFake) Provider() string { return "s3" }

func (f *multipartProbeFake) CreateMultipart(context.Context, string) (string, error) {
	f.record("create")
	if f.failAt == "create" {
		return "", errors.New(leakyErr)
	}
	return "upload-1", nil
}

func (f *multipartProbeFake) UploadPartURL(_ context.Context, _, uploadID string, part int32, _ time.Duration) (string, error) {
	f.record(fmt.Sprintf("sign %d", part))
	if f.failAt == "sign" {
		return "", errors.New(leakyErr)
	}
	return f.srv.URL + "/secret-bucket/probe?uploadId=" + uploadID + "&partNumber=" + strconv.Itoa(int(part)), nil
}

func (f *multipartProbeFake) CompleteMultipart(_ context.Context, _, _ string, partSize int64) (int64, error) {
	f.record(fmt.Sprintf("complete %d", partSize))
	if f.failAt == "complete" {
		return 0, errors.New(leakyErr)
	}
	return f.size(), nil
}

func (f *multipartProbeFake) size() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, s := range f.parts {
		n += s
	}
	return n
}

func (f *multipartProbeFake) AbortMultipart(context.Context, string, string) error {
	f.record("abort")
	return nil
}

func (f *multipartProbeFake) Stat(context.Context, string) (backupstorage.Object, error) {
	f.record("stat")
	if f.failAt == "stat" {
		return backupstorage.Object{}, errors.New(leakyErr)
	}
	if f.statSize != 0 {
		return backupstorage.Object{Size: f.statSize}, nil
	}
	return backupstorage.Object{Size: f.size()}, nil
}

func (f *multipartProbeFake) Delete(context.Context, string) error {
	f.record("delete")
	if f.failAt == "delete" {
		return errors.New(leakyErr)
	}
	return nil
}

func TestProbeBackupMultipart_SuccessRunsEveryStepInOrder(t *testing.T) {
	f := newMultipartProbeFake(t)
	res := probeBackupMultipart(context.Background(), f, multipartProbeClient)
	if !res.applicable || res.failedStep != "" {
		t.Fatalf("result = %+v, want applicable success", res)
	}
	minPart := backupstorage.MultipartMinPartSize
	want := []string{
		"create", "sign 1", "sign 2",
		fmt.Sprintf("put 1 len=%d cl=%d auth=\"\"", minPart, minPart),
		fmt.Sprintf("put 2 len=%d cl=%d auth=\"\"", multipartProbeLastPart, multipartProbeLastPart),
		fmt.Sprintf("complete %d", minPart),
		"stat", "delete",
	}
	if got := f.recorded(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(res.message(), "succeeded") {
		t.Errorf("message = %q", res.message())
	}
}

func TestProbeBackupMultipart_EachFailureAbortsDeletesAndNamesTheStep(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(*multipartProbeFake)
		step      string
		wantAbort bool
		wantInMsg string
	}{
		// No upload id exists to abort when Create itself failed.
		{"create", func(f *multipartProbeFake) { f.failAt = "create" }, probeStepCreate, false, "create step"},
		{"sign", func(f *multipartProbeFake) { f.failAt = "sign" }, probeStepSign, true, "sign step"},
		{"part upload", func(f *multipartProbeFake) { f.answerParts(http.StatusForbidden) }, probeStepUpload, true, "part upload step (HTTP 403)"},
		{"complete", func(f *multipartProbeFake) { f.failAt = "complete" }, probeStepComplete, true, "complete step"},
		{"verify stat", func(f *multipartProbeFake) { f.failAt = "stat" }, probeStepVerify, true, "verify step"},
		{"verify size", func(f *multipartProbeFake) { f.statSize = 5 }, probeStepVerify, true, "verify step"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newMultipartProbeFake(t)
			c.setup(f)
			res := probeBackupMultipart(context.Background(), f, multipartProbeClient)
			if !res.applicable || res.failedStep != c.step {
				t.Fatalf("result = %+v, want failure at %q", res, c.step)
			}
			if f.has("abort") != c.wantAbort {
				t.Errorf("abort called = %v, want %v (calls %q)", f.has("abort"), c.wantAbort, f.recorded())
			}
			if !f.has("delete") {
				t.Errorf("the probe object was not deleted after a failure (calls %q)", f.recorded())
			}
			msg := res.message()
			if !strings.Contains(msg, c.wantInMsg) {
				t.Errorf("message = %q, want it to contain %q", msg, c.wantInMsg)
			}
			for _, secret := range []string{"secret-bucket", "cloudflarestorage", "Signature", "deadbeef", "127.0.0.1"} {
				if strings.Contains(msg, secret) {
					t.Errorf("message leaks %q: %s", secret, msg)
				}
			}
			if c.step != probeStepComplete && c.step != probeStepVerify && f.has("complete") {
				t.Errorf("completed an upload whose %s step failed", c.step)
			}
		})
	}
}

func TestProbeBackupMultipart_CleanupFailureIsReported(t *testing.T) {
	f := newMultipartProbeFake(t)
	f.failAt = "delete"
	res := probeBackupMultipart(context.Background(), f, multipartProbeClient)
	if res.failedStep != probeStepCleanup || !strings.Contains(res.message(), "could not be deleted") {
		t.Fatalf("result = %+v / %q, want a cleanup failure", res, res.message())
	}
}

func TestProbeBackupMultipart_UnsupportedBackendIsNotApplicable(t *testing.T) {
	f := &probeFakeStorage{}
	res := probeBackupMultipart(context.Background(), f, multipartProbeClient)
	if res.applicable || res.failedStep != "" {
		t.Fatalf("result = %+v, want not applicable", res)
	}
	if !strings.Contains(res.message(), "not applicable") {
		t.Errorf("message = %q", res.message())
	}
}

// The endpoint is operator-typed; a redirect must not carry Core's PUT on.
func TestProbeBackupMultipart_DoesNotFollowARedirect(t *testing.T) {
	var elsewhere atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer target.Close()
	f := newMultipartProbeFake(t)
	redirect := http.RedirectHandler(target.URL, http.StatusTemporaryRedirect).ServeHTTP
	h := http.HandlerFunc(redirect)
	f.endpoint.Store(&h)

	res := probeBackupMultipart(context.Background(), f, multipartProbeClient)
	if res.failedStep != probeStepUpload || res.status != http.StatusTemporaryRedirect {
		t.Fatalf("result = %+v, want a part upload failure with 307", res)
	}
	if elsewhere.Load() != 0 {
		t.Fatalf("the redirect target received %d requests", elsewhere.Load())
	}
}

// The real probe against a real S3-compatible backend, through the same Open a
// backup storage row goes through, with a prefix as production's row has.
// Skipped unless DYLARIS_TEST_S3_ENDPOINT is set:
//
//	docker run -d --name dyl-minio -p 59004:9000 -e MINIO_ROOT_USER=minioadmin \
//	  -e MINIO_ROOT_PASSWORD=minioadmin minio/minio server /data
//	DYLARIS_TEST_S3_ENDPOINT=http://127.0.0.1:59004 go test -run MultipartProbeIntegration ./handlers
func TestMultipartProbeIntegration_MinIO(t *testing.T) {
	endpoint := os.Getenv("DYLARIS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("DYLARIS_TEST_S3_ENDPOINT not set")
	}
	ctx := context.Background()
	const bucket, prefix = "dylaris-f6-probe", "probe-prefix"

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
		"accessKeyId": "minioadmin", "secretAccessKey": "minioadmin", "prefix": prefix,
	})
	provider, err := backupstorage.Open(ctx, &models.BackupStorage{ID: 1, Name: "minio", Provider: "s3", Config: cfg}, backupstorage.Deps{})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	res := probeBackupMultipart(ctx, provider, multipartProbeClient)
	if !res.applicable || res.failedStep != "" {
		t.Fatalf("probe = %+v: %s", res, res.message())
	}
	t.Logf("multipart probe succeeded in %v: %s", time.Since(start), res.message())

	// Nothing left behind: no object, no open upload.
	objs, err := admin.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix + "/__dylaris_probe_")})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs.Contents) != 0 {
		t.Errorf("%d probe objects left in the bucket", len(objs.Contents))
	}
	ups, err := admin.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket), Prefix: aws.String(prefix + "/__dylaris_probe_")})
	if err != nil {
		t.Fatal(err)
	}
	if len(ups.Uploads) != 0 {
		t.Errorf("%d multipart uploads left open", len(ups.Uploads))
	}
}
