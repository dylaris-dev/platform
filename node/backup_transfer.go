package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "dylaris-proto/node"
)

// Backups and restores on object storage never give this node the bucket
// credentials, whoever owns the node. Core keeps them and hands out presigned
// URLs, asked for over the control stream when the transfer starts rather than
// minted at dispatch, so no URL ages in the command queue.
//
// A backup is a multipart upload Core owns: Core creates it, chooses a FIXED
// part size (R2 accepts nothing else), signs part URLs as this node reaches the
// parts, and completes it. A restore asks for one presigned GET.

// The values Core puts in a command to say the transfer goes through it.
const (
	modeUploadMultipart   = "multipart" // BackupRunCommand.Upload
	modeDownloadPresigned = "presigned" // BackupRestoreCommand.Download
)

const (
	// maxBufferedParts is how many parts are held in memory at once, which is
	// also how many upload in parallel: at Core's 64 MiB part size, about
	// 128 MiB for the length of a backup.
	maxBufferedParts = 2
	// partURLBatch is how many part URLs one request asks for. Core signs at
	// most 8; a URL is valid 15 minutes, so a batch far beyond what uploads in
	// parallel would mostly be re-requested after expiring.
	partURLBatch       = 4
	maxPartNumber      = 10000
	partUploadAttempts = 3
	// partPutTimeout bounds one part's PUT. Generous: a home uplink at 1 Mbit/s
	// shared by two parallel parts needs about 18 minutes for 64 MiB. Expiry of
	// the URL does not interrupt a PUT already sending; S3 checks it at start.
	partPutTimeout = 30 * time.Minute
	// maxNodePartSize refuses a part size this node would not hold twice in
	// memory, whatever Core says.
	maxNodePartSize = 256 << 20

	// transferRequestTimeout is one attempt of a URL request; MeshManager.Request
	// retries once on another Core.
	transferRequestTimeout = 30 * time.Second
	// completeRequestTimeout is one attempt of CompleteUpload. Core completes
	// within 8 minutes or answers with an error, so this waits for its answer
	// rather than asking a second replica while the first is still completing.
	completeRequestTimeout = 10 * time.Minute

	restoreStartAttempts = 3
)

// partRetryBackoff is multiplied by the attempt number. A variable so tests do
// not wait on it.
var partRetryBackoff = time.Second

// coreRequest sends a node-initiated request to Core. main sets it to the mesh
// manager's Request before any command is processed.
var coreRequest func(ctx context.Context, req *pb.NodeMessage, attemptTimeout time.Duration) (*pb.NodeMessage, error)

func askCore(ctx context.Context, req *pb.NodeMessage, timeout time.Duration) (*pb.NodeMessage, error) {
	if coreRequest == nil {
		return nil, errNoCoreConnection
	}
	return coreRequest(ctx, req, timeout)
}

// objectTransferClient carries presigned PUTs and GETs. No total timeout: a
// restore body may stream for hours. What is bounded is getting an answer at
// all - the dial and TLS handshake by the default transport, the response
// header here - so a backend that accepts and then says nothing fails the
// attempt instead of hanging the backup or the restore.
var objectTransferClient = &http.Client{Transport: func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 2 * time.Minute
	return t
}()}

// errNothingArchived is RunBackup's refusal to complete an archive whose
// include/exclude patterns matched no file.
var errNothingArchived = errors.New("no files matched include/exclude patterns")

// multipartAPI is what the uploader needs from Core, as functions so a test can
// play Core without a stream.
type multipartAPI struct {
	// partURLs returns the upload's part size and a URL per requested part.
	partURLs func(ctx context.Context, parts []int32) (int64, map[int32]string, error)
	// complete completes the upload and returns the archive's size.
	complete func(ctx context.Context) (int64, error)
}

// coreMultipartAPI asks Core, on the control stream, about backup run runID.
func coreMultipartAPI(runID int) multipartAPI {
	id := strconv.Itoa(runID)
	return multipartAPI{
		partURLs: func(ctx context.Context, parts []int32) (int64, map[int32]string, error) {
			resp, err := askCore(ctx, &pb.NodeMessage{Payload: &pb.NodeMessage_UploadPartUrlsRequest{
				UploadPartUrlsRequest: &pb.UploadPartUrlsRequest{RunId: id, PartNumbers: parts},
			}}, transferRequestTimeout)
			if err != nil {
				return 0, nil, fmt.Errorf("ask Core for part URLs: %w", err)
			}
			r := resp.GetUploadPartUrlsResponse()
			if r == nil {
				return 0, nil, errors.New("Core answered the part URL request with something else")
			}
			if r.Error != "" {
				return 0, nil, fmt.Errorf("Core refused part URLs: %s", r.Error)
			}
			urls := make(map[int32]string, len(r.Urls))
			for _, u := range r.Urls {
				urls[u.PartNumber] = u.Url
			}
			return r.PartSize, urls, nil
		},
		complete: func(ctx context.Context) (int64, error) {
			resp, err := askCore(ctx, &pb.NodeMessage{Payload: &pb.NodeMessage_CompleteUploadRequest{
				CompleteUploadRequest: &pb.CompleteUploadRequest{RunId: id},
			}}, completeRequestTimeout)
			if err != nil {
				return 0, fmt.Errorf("ask Core to complete the upload: %w", err)
			}
			r := resp.GetCompleteUploadResponse()
			if r == nil {
				return 0, errors.New("Core answered the complete request with something else")
			}
			if r.Error != "" {
				return 0, fmt.Errorf("Core could not complete the upload: %s", r.Error)
			}
			return r.SizeBytes, nil
		},
	}
}

// uploadMultipart streams r to Core's multipart upload and completes it,
// returning the size Core reports. beforeComplete runs once r is fully
// uploaded and can still refuse the archive; nothing is completed then, and
// Core aborts the parts when the run is reported failed.
//
// Part 1's URLs are asked for before the first byte is read, because that
// answer is where the part size comes from.
func uploadMultipart(ctx context.Context, api multipartAPI, client *http.Client, r io.Reader, beforeComplete func() error) (int64, error) {
	partSize, urls, err := api.partURLs(ctx, partBatch(1))
	if err != nil {
		return 0, err
	}
	if partSize <= 0 || partSize > maxNodePartSize {
		return 0, fmt.Errorf("Core chose a part size of %d bytes, outside 1..%d", partSize, maxNodePartSize)
	}
	if urls == nil {
		urls = map[int32]string{}
	}
	u := &partUploader{api: api, client: client, partSize: partSize, urls: urls}
	if err := u.uploadParts(ctx, r); err != nil {
		return 0, err
	}
	if beforeComplete != nil {
		if err := beforeComplete(); err != nil {
			return 0, err
		}
	}
	return api.complete(ctx)
}

func partBatch(from int32) []int32 {
	var out []int32
	for p := from; p < from+partURLBatch && p <= maxPartNumber; p++ {
		out = append(out, p)
	}
	return out
}

type partUploader struct {
	api      multipartAPI
	client   *http.Client
	partSize int64

	mu   sync.Mutex
	urls map[int32]string // signed but not yet used
}

// uploadParts cuts r into parts of exactly partSize bytes, the last one
// shorter or equal, and uploads them. A part is read only once a buffer is
// free, so at most maxBufferedParts are in memory - reading waits on the
// uploads rather than getting ahead of them.
func (u *partUploader) uploadParts(ctx context.Context, r io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	bufs := make(chan []byte, maxBufferedParts)
	for i := 0; i < maxBufferedParts; i++ {
		bufs <- nil // allocated on first use, so a small archive holds one
	}
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}

	var part int32
	for {
		var buf []byte
		select {
		case buf = <-bufs:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		if buf == nil {
			buf = make([]byte, u.partSize)
		}
		n, rerr := io.ReadFull(r, buf)
		if rerr == io.EOF {
			break // the previous part was the last, and it was full
		}
		if rerr != nil && rerr != io.ErrUnexpectedEOF {
			fail(fmt.Errorf("read archive: %w", rerr))
			break
		}
		if part == maxPartNumber {
			fail(fmt.Errorf("archive is larger than %d parts of %d bytes", maxPartNumber, u.partSize))
			break
		}
		part++
		wg.Add(1)
		go func(num int32, data, full []byte) {
			defer wg.Done()
			defer func() { bufs <- full }()
			if err := u.putPart(ctx, num, data); err != nil {
				fail(err)
			}
		}(part, buf[:n], buf)
		if rerr == io.ErrUnexpectedEOF {
			break // a short read is the last part
		}
	}
	wg.Wait()

	errMu.Lock()
	defer errMu.Unlock()
	if firstErr != nil {
		return firstErr
	}
	if part == 0 {
		// A tar.gz is never empty - gzip alone writes a header - so this is a
		// source that ended without saying why. Refused rather than completed:
		// an upload needs a part, and a zero-byte archive is not a backup.
		return errors.New("the archive produced no bytes")
	}
	return nil
}

// putPart uploads one part, retrying with a URL signed afresh each time, which
// covers an expired URL (403) as well as a failed transfer.
func (u *partUploader) putPart(ctx context.Context, num int32, data []byte) error {
	var lastErr error
	for attempt := 1; attempt <= partUploadAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(partRetryBackoff * time.Duration(attempt-1)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		target, err := u.url(ctx, num, attempt > 1)
		if err == nil {
			err = u.put(ctx, target, data)
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
	}
	return fmt.Errorf("part %d: %w", num, lastErr)
}

// url returns a signed URL for part num: one already signed, unless fresh is
// set, else a new batch from Core starting at num.
func (u *partUploader) url(ctx context.Context, num int32, fresh bool) (string, error) {
	if !fresh {
		u.mu.Lock()
		target, ok := u.urls[num]
		delete(u.urls, num)
		u.mu.Unlock()
		if ok {
			return target, nil
		}
	}
	batch := partBatch(num)
	if fresh {
		batch = []int32{num}
	}
	size, urls, err := u.api.partURLs(ctx, batch)
	if err != nil {
		return "", err
	}
	// The part size is fixed for the life of the upload; parts already cut to
	// the old size would fail at Complete.
	if size != u.partSize {
		return "", fmt.Errorf("Core changed the part size from %d to %d during the upload", u.partSize, size)
	}
	target, ok := urls[num]
	if !ok {
		return "", fmt.Errorf("Core signed no URL for part %d", num)
	}
	u.mu.Lock()
	for p, v := range urls {
		if p != num {
			u.urls[p] = v
		}
	}
	u.mu.Unlock()
	return target, nil
}

// put sends one part with a known Content-Length and nothing else signed.
func (u *partUploader) put(ctx context.Context, target string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, partPutTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build part request: %w", withoutURL(err))
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("put: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("put status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// withoutURL drops the request URL net/http puts into its errors. A presigned
// URL names the bucket's endpoint and carries a live signature, and the error
// ends up in a run message the server's owner can read.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// coreRestoreURL asks Core for the presigned GET of restore restoreID.
func coreRestoreURL(restoreID int) func(ctx context.Context) (string, error) {
	id := strconv.Itoa(restoreID)
	return func(ctx context.Context) (string, error) {
		resp, err := askCore(ctx, &pb.NodeMessage{Payload: &pb.NodeMessage_RestoreUrlRequest{
			RestoreUrlRequest: &pb.RestoreUrlRequest{RestoreId: id},
		}}, transferRequestTimeout)
		if err != nil {
			return "", fmt.Errorf("ask Core for the restore URL: %w", err)
		}
		r := resp.GetRestoreUrlResponse()
		if r == nil {
			return "", errors.New("Core answered the restore URL request with something else")
		}
		if r.Error != "" {
			return "", fmt.Errorf("Core refused the restore URL: %s", r.Error)
		}
		if r.Url == "" {
			return "", errors.New("Core returned an empty restore URL")
		}
		return r.Url, nil
	}
}

// openPresignedRestore starts the download of a restore archive. first is a URL
// already asked for; each retry asks Core for a fresh one, since an expired URL
// and a failed start look the same from here. Only the START is retried: once
// the body streams, a failure mid-archive belongs to the extraction.
func openPresignedRestore(ctx context.Context, client *http.Client, first string, getURL func(context.Context) (string, error)) (io.ReadCloser, error) {
	var lastErr error
	target := first
	for attempt := 1; attempt <= restoreStartAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(partRetryBackoff * time.Duration(attempt-1)):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			fresh, err := getURL(ctx)
			if err != nil {
				lastErr = err
				continue
			}
			target = fresh
		}
		body, err := downloadPresigned(ctx, client, target)
		if err == nil {
			return body, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
	}
	return nil, lastErr
}
