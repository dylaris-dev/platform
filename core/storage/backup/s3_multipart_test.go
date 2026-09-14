package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const mib = int64(1 << 20)

func TestValidateParts(t *testing.T) {
	const size = 5 * mib
	tests := []struct {
		name     string
		parts    []partInfo
		partSize int64
		wantErr  bool
	}{
		{"equal parts and a shorter last part", []partInfo{{1, size}, {2, size}, {3, 10}}, size, false},
		{"equal parts and an equal last part", []partInfo{{1, size}, {2, size}}, size, false},
		{"single part smaller than the part size is a small archive", []partInfo{{1, 123}}, size, false},
		{"zero parts means nothing was uploaded", nil, size, true},
		{"gap in the numbering", []partInfo{{1, size}, {3, 10}}, size, true},
		{"numbering does not start at 1", []partInfo{{2, size}, {3, 10}}, size, true},
		{"duplicate part number", []partInfo{{1, size}, {1, size}, {2, 10}}, size, true},
		{"middle part one byte too large", []partInfo{{1, size + 1}, {2, 10}}, size, true},
		{"middle part smaller than the part size", []partInfo{{1, size}, {2, size - 1}, {3, 10}}, size, true},
		{"last part larger than the part size", []partInfo{{1, size}, {2, size + 1}}, size, true},
		{"single part larger than the part size", []partInfo{{1, size + 1}}, size, true},
		{"part size below 5 MiB", []partInfo{{1, 10}}, MultipartMinPartSize - 1, true},
		{"part size above 5 GiB", []partInfo{{1, 10}}, MultipartMaxPartSize + 1, true},
		{"part size of exactly 5 GiB", []partInfo{{1, 10}}, MultipartMaxPartSize, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateParts(tt.parts, tt.partSize)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateParts(%v, %d) = %v, wantErr %v", tt.parts, tt.partSize, err, tt.wantErr)
			}
		})
	}
}

// MinIO answers an abort of a missing upload with 204, so the integration test
// cannot reach this branch; AWS and R2 answer NoSuchUpload (404).
func TestIsNoSuchUpload(t *testing.T) {
	notFound := &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusNotFound}},
		Err:      errors.New("not found"),
	}}
	forbidden := &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusForbidden}},
		Err:      errors.New("forbidden"),
	}}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"typed NoSuchUpload", &types.NoSuchUpload{}, true},
		{"generic API error code", &smithy.GenericAPIError{Code: "NoSuchUpload"}, true},
		{"bare 404 response", notFound, true},
		{"403 is a refusal, not a missing upload", forbidden, false},
		{"access denied", &smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{"transport error", errors.New("connection refused"), false},
	}
	for _, tt := range tests {
		if got := isNoSuchUpload(tt.err); got != tt.want {
			t.Errorf("%s: isNoSuchUpload = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// Backends with no object storage must refuse all four multipart operations
// with the sentinel, so a caller can tell "this target cannot take a node's
// parts" from a failed request.
func TestMultipartUnsupportedBackends(t *testing.T) {
	ctx := context.Background()
	for _, st := range []Storage{&LocalStorage{}, &NodeLocalStorage{}} {
		if _, err := st.CreateMultipart(ctx, "k"); !errors.Is(err, ErrMultipartUnsupported) {
			t.Errorf("%T CreateMultipart = %v, want ErrMultipartUnsupported", st, err)
		}
		if _, err := st.UploadPartURL(ctx, "k", "u", 1, time.Minute); !errors.Is(err, ErrMultipartUnsupported) {
			t.Errorf("%T UploadPartURL = %v, want ErrMultipartUnsupported", st, err)
		}
		if _, err := st.CompleteMultipart(ctx, "k", "u", 5*mib); !errors.Is(err, ErrMultipartUnsupported) {
			t.Errorf("%T CompleteMultipart = %v, want ErrMultipartUnsupported", st, err)
		}
		if err := st.AbortMultipart(ctx, "k", "u"); !errors.Is(err, ErrMultipartUnsupported) {
			t.Errorf("%T AbortMultipart = %v, want ErrMultipartUnsupported", st, err)
		}
	}
}

// newIntegrationS3 connects to the S3-compatible endpoint named by
// DYLARIS_TEST_S3_ENDPOINT and makes sure the test bucket exists. Skipped when
// the variable is unset, so `go test ./...` stays green without one.
//
// Run locally against MinIO:
//
//	docker run -d --name dyl-minio -p 59000:9000 \
//	  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin minio/minio server /data
//	DYLARIS_TEST_S3_ENDPOINT=http://127.0.0.1:59000 go test ./storage/backup/ -run Integration -v
func newIntegrationS3(t *testing.T, prefix string) *S3Storage {
	t.Helper()
	endpoint := os.Getenv("DYLARIS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("DYLARIS_TEST_S3_ENDPOINT not set - skipping the S3 multipart integration tests")
	}
	env := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}
	raw, err := json.Marshal(S3Config{
		Endpoint:        endpoint,
		Region:          env("DYLARIS_TEST_S3_REGION", "us-east-1"),
		Bucket:          env("DYLARIS_TEST_S3_BUCKET", "dylaris-test"),
		AccessKeyID:     env("DYLARIS_TEST_S3_ACCESS_KEY", "minioadmin"),
		SecretAccessKey: env("DYLARIS_TEST_S3_SECRET_KEY", "minioadmin"),
		ForcePathStyle:  true,
		Prefix:          prefix,
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	st, err := NewS3(context.Background(), raw)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	_, err = st.client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(st.bucket)})
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
		t.Fatalf("create bucket %s: %v", st.bucket, err)
	}
	return st
}

// putPart uploads one part the way a node will: a plain PUT to the presigned
// URL with a known Content-Length and no other headers, no SDK involved.
func putPart(t *testing.T, url string, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build part request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT part: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT part status = %d, body %s", resp.StatusCode, msg)
	}
}

func patterned(n int64, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

// uploadTwoParts creates an upload and sends part 1 and part 2 through
// presigned URLs.
func uploadTwoParts(t *testing.T, st *S3Storage, key string, part1, part2 []byte) string {
	t.Helper()
	ctx := context.Background()
	id, err := st.CreateMultipart(ctx, key)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	for i, body := range [][]byte{part1, part2} {
		url, err := st.UploadPartURL(ctx, key, id, int32(i+1), 5*time.Minute)
		if err != nil {
			t.Fatalf("UploadPartURL(%d): %v", i+1, err)
		}
		putPart(t, url, body)
	}
	return id
}

func testKey(name string) string {
	return "multipart-it/" + strconv.FormatInt(time.Now().UnixNano(), 36) + "/" + name
}

func TestS3MultipartIntegration_PresignedPartsRoundTrip(t *testing.T) {
	st := newIntegrationS3(t, "")
	ctx := context.Background()
	key := testKey("archive.tar.gz")
	t.Cleanup(func() { _ = st.Delete(context.Background(), key) })

	part1, part2 := patterned(5*mib, 1), patterned(1234, 7)
	id := uploadTwoParts(t, st, key, part1, part2)

	size, err := st.CompleteMultipart(ctx, key, id, 5*mib)
	if err != nil {
		t.Fatalf("CompleteMultipart: %v", err)
	}
	if want := int64(len(part1) + len(part2)); size != want {
		t.Errorf("CompleteMultipart size = %d, want %d", size, want)
	}
	rc, err := st.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	if !bytes.Equal(got, append(append([]byte{}, part1...), part2...)) {
		t.Fatalf("object bytes differ from the uploaded parts (got %d bytes)", len(got))
	}
}

// A middle part of the wrong size must be refused by Core BEFORE the backend
// is asked to complete: afterwards the upload still exists with both parts and
// no object was written. Then abort works, and aborting again is not an error.
func TestS3MultipartIntegration_RefusesUnequalPartsWithoutCompleting(t *testing.T) {
	st := newIntegrationS3(t, "")
	ctx := context.Background()
	key := testKey("unequal.tar.gz")
	t.Cleanup(func() { _ = st.Delete(context.Background(), key) })

	id := uploadTwoParts(t, st, key, patterned(5*mib+1, 3), patterned(100, 9))

	usage, err := st.ListMultipart(ctx, key, id)
	if err != nil {
		t.Fatalf("ListMultipart: %v", err)
	}
	if want := (MultipartUsage{Parts: 2, Bytes: 5*mib + 1 + 100, Largest: 5*mib + 1}); usage != want {
		t.Errorf("ListMultipart = %+v, want %+v", usage, want)
	}

	if _, err := st.CompleteMultipart(ctx, key, id, 5*mib); err == nil {
		t.Fatal("CompleteMultipart with a 5 MiB+1 middle part = nil, want the part-size refusal")
	}
	parts, err := st.client.ListParts(ctx, &s3.ListPartsInput{
		Bucket: aws.String(st.bucket), Key: aws.String(st.key(key)), UploadId: aws.String(id),
	})
	if err != nil {
		t.Fatalf("ListParts after the refusal: %v (the upload must still exist)", err)
	}
	if len(parts.Parts) != 2 {
		t.Errorf("parts after the refusal = %d, want 2", len(parts.Parts))
	}
	if _, err := st.Stat(ctx, key); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat after the refusal = %v, want fs.ErrNotExist (nothing completed)", err)
	}

	if err := st.AbortMultipart(ctx, key, id); err != nil {
		t.Fatalf("AbortMultipart: %v", err)
	}
	if err := st.AbortMultipart(ctx, key, id); err != nil {
		t.Errorf("second AbortMultipart = %v, want nil for an upload that is already gone", err)
	}
}

// A prefixed storage completes at the namespaced key: its own Get reads the
// object back by the caller's key, and an unprefixed view of the same bucket
// finds it under prefix + key.
func TestS3MultipartIntegration_PrefixNamespacesTheCompletedObject(t *testing.T) {
	prefixed := newIntegrationS3(t, "server-backups")
	root := newIntegrationS3(t, "")
	ctx := context.Background()
	key := testKey("prefixed.tar.gz")
	t.Cleanup(func() { _ = prefixed.Delete(context.Background(), key) })

	part1, part2 := patterned(5*mib, 5), patterned(10, 11)
	id := uploadTwoParts(t, prefixed, key, part1, part2)
	if _, err := prefixed.CompleteMultipart(ctx, key, id, 5*mib); err != nil {
		t.Fatalf("CompleteMultipart: %v", err)
	}

	obj, err := prefixed.Stat(ctx, key)
	if err != nil {
		t.Fatalf("prefixed Stat(%s): %v", key, err)
	}
	if want := int64(len(part1) + len(part2)); obj.Size != want {
		t.Errorf("prefixed Stat size = %d, want %d", obj.Size, want)
	}
	rc, err := prefixed.Get(ctx, key)
	if err != nil {
		t.Fatalf("prefixed Get: %v", err)
	}
	rc.Close()
	if _, err := root.Stat(ctx, "server-backups/"+key); err != nil {
		t.Errorf("root Stat(server-backups/%s) = %v, want the object under the prefix", key, err)
	}
	if _, err := root.Stat(ctx, key); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("root Stat(%s) = %v, want fs.ErrNotExist (the prefix was not applied)", key, err)
	}
}
