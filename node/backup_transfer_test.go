package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-pkg/queue"
	pb "dylaris-proto/node"
)

// partBucket plays the presigned endpoint: it stores every PUT by part number,
// and fail decides, per request, whether to answer with an error status instead.
type partBucket struct {
	mu    sync.Mutex
	parts map[int32][]byte
	// fail returns a status to answer with, or 0 to accept the part.
	fail func(part int32, query string) int
	// gate, when set, is waited on before each PUT is answered.
	gate     chan struct{}
	inFlight atomic.Int32
	peak     atomic.Int32
	arrived  chan int32
}

func newPartBucket() *partBucket {
	return &partBucket{parts: map[int32][]byte{}, arrived: make(chan int32, 64)}
}

func (b *partBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := b.inFlight.Add(1)
	defer b.inFlight.Add(-1)
	for {
		p := b.peak.Load()
		if n <= p || b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	part64, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
	part := int32(part64)
	body, _ := io.ReadAll(r.Body)
	if r.ContentLength != int64(len(body)) {
		http.Error(w, "content length mismatch", http.StatusBadRequest)
		return
	}
	select {
	case b.arrived <- part:
	default:
	}
	if b.gate != nil {
		<-b.gate
	}
	if b.fail != nil {
		if status := b.fail(part, r.URL.RawQuery); status != 0 {
			w.WriteHeader(status)
			return
		}
	}
	b.mu.Lock()
	b.parts[part] = body
	b.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// assembled returns the stored parts in order with their sizes.
func (b *partBucket) assembled() ([]byte, []int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var nums []int
	for n := range b.parts {
		nums = append(nums, int(n))
	}
	sort.Ints(nums)
	var all []byte
	var sizes []int
	for i, n := range nums {
		if n != i+1 {
			return nil, nil // a gap
		}
		all = append(all, b.parts[int32(n)]...)
		sizes = append(sizes, len(b.parts[int32(n)]))
	}
	return all, sizes
}

// fakeTransferCore signs URLs against a partBucket and counts what it was asked.
type fakeTransferCore struct {
	url      string
	partSize int64
	bucket   *partBucket

	mu        sync.Mutex
	requests  [][]int32
	completes int
}

func (c *fakeTransferCore) api() multipartAPI {
	return multipartAPI{
		partURLs: func(_ context.Context, parts []int32) (int64, map[int32]string, error) {
			c.mu.Lock()
			c.requests = append(c.requests, append([]int32(nil), parts...))
			gen := len(c.requests)
			c.mu.Unlock()
			urls := map[int32]string{}
			for _, p := range parts {
				urls[p] = fmt.Sprintf("%s/archive?partNumber=%d&gen=%d", c.url, p, gen)
			}
			return c.partSize, urls, nil
		},
		complete: func(context.Context) (int64, error) {
			c.mu.Lock()
			c.completes++
			c.mu.Unlock()
			all, _ := c.bucket.assembled()
			return int64(len(all)), nil
		},
	}
}

func (c *fakeTransferCore) completeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.completes
}

func newFakeCore(t *testing.T, partSize int64) *fakeTransferCore {
	t.Helper()
	bucket := newPartBucket()
	srv := httptest.NewServer(bucket)
	t.Cleanup(srv.Close)
	return &fakeTransferCore{url: srv.URL, partSize: partSize, bucket: bucket}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

func init() { partRetryBackoff = time.Millisecond }

// Every part but the last is exactly the part size, the parts are numbered 1..N
// without gaps, the bytes reassemble to the input, and Complete runs once. An
// input that is an exact multiple ends on a full part, not an empty extra one.
func TestUploadMultipart_CutsFixedSizeParts(t *testing.T) {
	const partSize = 1024
	for _, tc := range []struct {
		name  string
		size  int
		sizes []int
	}{
		{"with a short last part", 3*partSize + 300, []int{partSize, partSize, partSize, 300}},
		{"an exact multiple", 3 * partSize, []int{partSize, partSize, partSize}},
		{"smaller than one part", 10, []int{10}},
		{"larger than one URL batch", 9 * partSize, []int{partSize, partSize, partSize, partSize, partSize, partSize, partSize, partSize, partSize}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core := newFakeCore(t, partSize)
			data := randomBytes(tc.size)

			size, err := uploadMultipart(context.Background(), core.api(), http.DefaultClient, bytes.NewReader(data), nil)
			if err != nil {
				t.Fatalf("uploadMultipart: %v", err)
			}
			got, sizes := core.bucket.assembled()
			if fmt.Sprint(sizes) != fmt.Sprint(tc.sizes) {
				t.Fatalf("part sizes = %v, want %v", sizes, tc.sizes)
			}
			if !bytes.Equal(got, data) {
				t.Fatal("the parts do not reassemble to the input")
			}
			if core.completeCount() != 1 || size != int64(tc.size) {
				t.Fatalf("completes = %d size = %d, want 1 / %d", core.completeCount(), size, tc.size)
			}
			for _, req := range core.requests {
				if len(req) > partURLBatch {
					t.Errorf("asked for %d URLs at once, want at most %d", len(req), partURLBatch)
				}
			}
		})
	}
}

// countingSource reports how far the uploader has read.
type countingSource struct {
	r    io.Reader
	read atomic.Int64
}

func (c *countingSource) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// With both uploads stalled, the uploader must not read a third part: the
// archive is held in at most two part buffers, never more.
func TestUploadMultipart_HoldsAtMostTwoPartsInMemory(t *testing.T) {
	const partSize = 4096
	core := newFakeCore(t, partSize)
	core.bucket.gate = make(chan struct{})
	// Registered after the server's Close, so it runs first: a failure below
	// must not leave handlers blocked on the gate for Close to wait on forever.
	var release sync.Once
	openGate := func() { release.Do(func() { close(core.bucket.gate) }) }
	t.Cleanup(openGate)
	src := &countingSource{r: bytes.NewReader(randomBytes(6 * partSize))}

	done := make(chan error, 1)
	go func() {
		_, err := uploadMultipart(context.Background(), core.api(), http.DefaultClient, src, nil)
		done <- err
	}()

	for i := 0; i < maxBufferedParts; i++ {
		select {
		case <-core.bucket.arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("the first parts never arrived")
		}
	}
	time.Sleep(100 * time.Millisecond) // time to get ahead, if it would
	if got := src.read.Load(); got > maxBufferedParts*partSize {
		t.Fatalf("read %d bytes while two uploads were stalled, want at most %d", got, maxBufferedParts*partSize)
	}
	openGate()
	if err := <-done; err != nil {
		t.Fatalf("uploadMultipart: %v", err)
	}
	if peak := core.bucket.peak.Load(); peak > maxBufferedParts {
		t.Fatalf("peak parallel uploads = %d, want at most %d", peak, maxBufferedParts)
	}
}

// An expired URL answers 403. The part is sent again with a URL Core signs for
// that part alone, and the upload completes.
func TestUploadMultipart_RetriesAPartWithAFreshURL(t *testing.T) {
	const partSize = 1024
	core := newFakeCore(t, partSize)
	core.bucket.fail = func(part int32, query string) int {
		if part == 2 && strings.Contains(query, "gen=1") {
			return http.StatusForbidden // the first batch's URL for part 2 "expired"
		}
		return 0
	}
	data := randomBytes(3*partSize + 1)

	if _, err := uploadMultipart(context.Background(), core.api(), http.DefaultClient, bytes.NewReader(data), nil); err != nil {
		t.Fatalf("uploadMultipart: %v", err)
	}
	got, _ := core.bucket.assembled()
	if !bytes.Equal(got, data) {
		t.Fatal("the parts do not reassemble to the input after the retry")
	}
	fresh := false
	for _, req := range core.requests {
		if len(req) == 1 && req[0] == 2 {
			fresh = true
		}
	}
	if !fresh {
		t.Fatalf("requests = %v, want a fresh single URL for part 2", core.requests)
	}
}

// A part that keeps failing fails the upload after the bounded attempts, and
// nothing is completed.
func TestUploadMultipart_AFailingPartNeverCompletes(t *testing.T) {
	core := newFakeCore(t, 1024)
	var attempts atomic.Int32
	core.bucket.fail = func(part int32, _ string) int {
		if part == 3 {
			attempts.Add(1)
			return http.StatusInternalServerError
		}
		return 0
	}
	_, err := uploadMultipart(context.Background(), core.api(), http.DefaultClient, bytes.NewReader(randomBytes(5*1024)), nil)
	if err == nil {
		t.Fatal("a part that always fails did not fail the upload")
	}
	if core.completeCount() != 0 {
		t.Fatal("Complete was called after a failed part")
	}
	if got := attempts.Load(); got != partUploadAttempts {
		t.Errorf("part 3 attempts = %d, want %d", got, partUploadAttempts)
	}
	if strings.Contains(err.Error(), core.url) {
		t.Errorf("error %q carries the presigned URL", err)
	}
}

func TestUploadMultipart_RefusesAnEmptyArchiveAndARefusedOne(t *testing.T) {
	core := newFakeCore(t, 1024)
	if _, err := uploadMultipart(context.Background(), core.api(), http.DefaultClient, bytes.NewReader(nil), nil); err == nil {
		t.Error("an empty source was uploaded")
	}
	refusal := errors.New("nothing matched")
	_, err := uploadMultipart(context.Background(), core.api(), http.DefaultClient, bytes.NewReader(randomBytes(100)),
		func() error { return refusal })
	if !errors.Is(err, refusal) {
		t.Errorf("err = %v, want the beforeComplete refusal", err)
	}
	if core.completeCount() != 0 {
		t.Error("Complete was called for a refused archive")
	}
}

// runBackupWithFakeCore drives the real RunBackup against a fake Core on the
// control stream and returns the terminal report.
func runBackupWithFakeCore(t *testing.T, core *fakeTransferCore, include []string) map[string]interface{} {
	t.Helper()
	prevNode, prevReq := nodeID, coreRequest
	t.Cleanup(func() { nodeID, coreRequest = prevNode, prevReq })
	nodeID = "node-f3"
	api := core.api()
	coreRequest = func(ctx context.Context, req *pb.NodeMessage, _ time.Duration) (*pb.NodeMessage, error) {
		switch {
		case req.GetUploadPartUrlsRequest() != nil:
			size, urls, _ := api.partURLs(ctx, req.GetUploadPartUrlsRequest().PartNumbers)
			resp := &pb.UploadPartUrlsResponse{UploadId: "u", PartSize: size}
			for p, u := range urls {
				resp.Urls = append(resp.Urls, &pb.PartUrl{PartNumber: p, Url: u})
			}
			return &pb.NodeMessage{Payload: &pb.NodeMessage_UploadPartUrlsResponse{UploadPartUrlsResponse: resp}}, nil
		case req.GetCompleteUploadRequest() != nil:
			size, _ := api.complete(ctx)
			return &pb.NodeMessage{Payload: &pb.NodeMessage_CompleteUploadResponse{CompleteUploadResponse: &pb.CompleteUploadResponse{SizeBytes: size}}}, nil
		}
		return nil, errors.New("unexpected request")
	}

	root := t.TempDir()
	const uuid = "srv-f3"
	if err := os.MkdirAll(filepath.Join(root, uuid, "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, uuid, "world", "level.dat"), randomBytes(5000), 0o644); err != nil {
		t.Fatal(err)
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	sub := rdb.Subscribe(context.Background(), queue.BackupResultsChannel(nodeID))
	t.Cleanup(func() { sub.Close() })
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}

	RunBackup(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, BackupRunCommand{
		RunID: 77, JobID: 1, ServerUUID: uuid, IncludePatterns: include,
		StorageKey: "backups/srv-f3/job-1/x.tar.gz",
		Storage:    json.RawMessage(`{"id":3,"provider":"connection","config":{}}`),
		Upload:     modeUploadMultipart,
	})

	for {
		msg, err := sub.ReceiveTimeout(context.Background(), 5*time.Second)
		if err != nil {
			t.Fatalf("no report: %v", err)
		}
		m, ok := msg.(*redis.Message)
		if !ok {
			continue
		}
		var report map[string]interface{}
		_ = json.Unmarshal([]byte(m.Payload), &report)
		if report["status"] != "running" {
			return report
		}
	}
}

func TestRunBackup_MultipartSuccessReportsCoresSize(t *testing.T) {
	core := newFakeCore(t, 1024)
	report := runBackupWithFakeCore(t, core, nil)
	if report["status"] != "success" {
		t.Fatalf("report = %v, want success", report)
	}
	all, _ := core.bucket.assembled()
	if report["sizeBytes"] != float64(len(all)) || core.completeCount() != 1 {
		t.Fatalf("size = %v completes = %d, want %d / 1", report["sizeBytes"], core.completeCount(), len(all))
	}
	gz, err := gzip.NewReader(bytes.NewReader(all))
	if err != nil {
		t.Fatalf("the uploaded parts are not a gzip stream: %v", err)
	}
	tr := tar.NewReader(gz)
	found := false
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Name == "world/level.dat" {
			found = true
		}
	}
	if !found {
		t.Fatal("the uploaded archive does not contain world/level.dat")
	}
}

func TestRunBackup_MultipartFailureReportsFailedWithoutCompleting(t *testing.T) {
	core := newFakeCore(t, 1024)
	core.bucket.fail = func(int32, string) int { return http.StatusInternalServerError }
	report := runBackupWithFakeCore(t, core, nil)
	if report["status"] != "failed" || !strings.Contains(fmt.Sprint(report["error"]), "upload failed") {
		t.Fatalf("report = %v, want an upload failure", report)
	}
	if core.completeCount() != 0 {
		t.Fatal("Complete was called for a failed upload")
	}
}

func TestRunBackup_MultipartNothingMatchedIsNeverCompleted(t *testing.T) {
	core := newFakeCore(t, 1024)
	report := runBackupWithFakeCore(t, core, []string{"no-such-dir/**"})
	if report["status"] != "failed" || report["error"] != errNothingArchived.Error() {
		t.Fatalf("report = %v, want the nothing-matched failure", report)
	}
	if core.completeCount() != 0 {
		t.Fatal("Complete was called for an archive that matched nothing")
	}
}

// The restore start is retried with a fresh URL; the first URL's failure does
// not end the restore.
func TestOpenPresignedRestore_RetriesWithAFreshURL(t *testing.T) {
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if r.URL.Query().Get("fresh") != "1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("archive"))
	}))
	defer srv.Close()

	asked := 0
	body, err := openPresignedRestore(context.Background(), objectTransferClient, srv.URL+"/a?fresh=0",
		func(context.Context) (string, error) { asked++; return srv.URL + "/a?fresh=1", nil })
	if err != nil {
		t.Fatalf("openPresignedRestore: %v", err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != "archive" || asked != 1 || gets.Load() != 2 {
		t.Fatalf("body %q, URL asked %d times, %d GETs; want archive / 1 / 2", got, asked, gets.Load())
	}

	// And it gives up after the bounded attempts, without the URL in the error.
	_, err = openPresignedRestore(context.Background(), objectTransferClient, srv.URL+"/a?fresh=0",
		func(context.Context) (string, error) { return srv.URL + "/a?fresh=0", nil })
	if err == nil {
		t.Fatal("a download that always fails was opened")
	}
}
