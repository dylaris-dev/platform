package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
)

// backupStorageProbePayload is the fixed content the round-trip probe writes and
// reads back. Reading it back and comparing is what turns "writable" into
// "reachable and consistent": a backend that accepts a write but hands back
// different or no bytes is broken, and the old put-then-delete probe reported it
// as green.
const backupStorageProbePayload = "dylaris-backup-probe"

// probeBackupStorage writes, reads back and deletes a uniquely-named probe
// object to verify a backup backend is reachable AND read/write-consistent, not
// merely writable. It deletes the probe on every return path - write error, read
// error, mismatch - so a broken candidate never leaves a stray object behind.
// Mirrors probeStorageProvider for core storage.
func probeBackupStorage(ctx context.Context, provider backupstorage.Storage) (ok bool, message string) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	key := "__dylaris_probe_" + hex.EncodeToString(b) + ".txt"

	if err := provider.Put(ctx, key, strReader(backupStorageProbePayload), int64(len(backupStorageProbePayload))); err != nil {
		_ = provider.Delete(ctx, key)
		return false, "put failed: " + err.Error()
	}
	rc, err := provider.Get(ctx, key)
	if err != nil {
		_ = provider.Delete(ctx, key)
		return false, "read-back failed: " + err.Error()
	}
	got, readErr := io.ReadAll(rc)
	rc.Close()
	if readErr != nil {
		_ = provider.Delete(ctx, key)
		return false, "read-back failed: " + readErr.Error()
	}
	if string(got) != backupStorageProbePayload {
		_ = provider.Delete(ctx, key)
		return false, "read-back mismatch: storage backend is not consistent"
	}
	if err := provider.Delete(ctx, key); err != nil {
		return false, "cleanup failed: " + err.Error()
	}
	return true, "Storage reachable: write, read and delete all succeeded."
}

// backupStorageEphemeralWarning returns a non-empty warning when a local/shared
// backup path sits on the container's own filesystem instead of a mounted
// volume, meaning every archive written there is LOST on the next container
// recreation. It mirrors the core-storage ephemeral-path warning and goes
// through the same pathOnContainerRootFS seam. Any other provider, an
// unparseable config, or a path the check cannot judge (a non-Linux host)
// returns "". A silently-ephemeral backup target is worse than an ephemeral core
// path: the operator believes backups exist when they do not.
func backupStorageEphemeralWarning(s *models.BackupStorage) string {
	if s.Provider != "local" && s.Provider != "shared" {
		return ""
	}
	var cfg backupstorage.LocalConfig
	if err := json.Unmarshal(s.Config, &cfg); err != nil || cfg.BasePath == "" {
		return ""
	}
	if onRoot, determinable := pathOnContainerRootFS(cfg.BasePath); determinable && onRoot {
		return "This backup path is on the container's own filesystem, not a mounted volume, so every archive written here is LOST when the container is recreated. The read/write test above still passes because that directory is genuinely writable. Add a bind mount or volume for this path in your compose/stack file, or point it at a directory inside a volume that is already mounted."
	}
	return ""
}

// The multipart probe proves, with Core's own credentials, the path every node
// backup on object storage now takes: Core creates a multipart upload, signs
// part URLs, a plain HTTP client PUTs the parts, Core completes it. The round
// trip above cannot show that - a backend can take PutObject and still refuse a
// presigned UploadPart or unequal parts - and no node needs to be involved, so
// a storage that holds live backups can be checked without anyone reading its
// key.
const (
	multipartProbeTimeout = 2 * time.Minute
	// multipartProbeCleanupTimeout gives the abort and delete their own budget,
	// so a probe that ran out of time still cleans up.
	multipartProbeCleanupTimeout = 30 * time.Second
	multipartProbeURLTTL         = 5 * time.Minute
	// multipartProbeLastPart is the short final part; part 1 is exactly
	// MultipartMinPartSize, the smallest part every backend accepts before the last.
	multipartProbeLastPart = 1024
)

// The steps a multipart probe can fail at, as the panel shows them.
const (
	probeStepCreate   = "create"
	probeStepSign     = "sign"
	probeStepUpload   = "part upload"
	probeStepComplete = "complete"
	probeStepVerify   = "verify"
	probeStepCleanup  = "cleanup"
)

// multipartProbeClient sends the parts the way a node does: plain net/http, a
// known Content-Length, no other header. It does not follow a redirect, because
// the endpoint is typed by an operator and a redirect would carry Core's
// request to wherever it points.
var multipartProbeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// multipartProbeResult is what probeBackupMultipart found. failedStep is empty
// on success; status is the part PUT's HTTP status when that step got an answer.
type multipartProbeResult struct {
	applicable bool
	failedStep string
	status     int
}

// message is the sentence the panel shows. It names the step and nothing the
// storage said: an error from the backend or the HTTP client carries the
// endpoint, the bucket or a signed URL, and those go to the log only.
func (r multipartProbeResult) message() string {
	switch {
	case !r.applicable:
		return "Multipart upload: not applicable to this storage."
	case r.failedStep == "":
		return "Multipart upload through presigned part URLs succeeded, which is how nodes upload backups."
	case r.failedStep == probeStepCleanup:
		return "Multipart upload through presigned part URLs succeeded, but its probe object could not be deleted. " +
			"The Core log has the details."
	}
	detail := ""
	if r.failedStep == probeStepUpload {
		if r.status != 0 {
			detail = fmt.Sprintf(" (HTTP %d)", r.status)
		} else {
			detail = " (no answer)"
		}
	}
	return fmt.Sprintf("Single objects work, but a multipart upload through presigned part URLs failed at the %s step%s. "+
		"Nodes upload backups this way, so backups to this storage will fail. The Core log has the details.", r.failedStep, detail)
}

// probeBackupMultipart runs the multipart probe against provider and removes
// what it created on every path. A backend without multipart is reported as not
// applicable rather than failed.
func probeBackupMultipart(ctx context.Context, provider backupstorage.Storage, client *http.Client) multipartProbeResult {
	ctx, cancel := context.WithTimeout(ctx, multipartProbeTimeout)
	defer cancel()

	b := make([]byte, 8)
	_, _ = rand.Read(b)
	key := "__dylaris_probe_" + hex.EncodeToString(b) + ".multipart"

	uploadID, err := provider.CreateMultipart(ctx, key)
	if errors.Is(err, backupstorage.ErrMultipartUnsupported) {
		return multipartProbeResult{applicable: false}
	}
	res := multipartProbeResult{applicable: true}
	fail := func(step string, err error) multipartProbeResult {
		log.Printf("backup storage multipart probe: %s failed: %v", step, err)
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), multipartProbeCleanupTimeout)
		defer ccancel()
		if uploadID != "" {
			if aerr := provider.AbortMultipart(cctx, key, uploadID); aerr != nil {
				log.Printf("backup storage multipart probe: abort after %s failed: %v", step, aerr)
			}
		}
		if derr := provider.Delete(cctx, key); derr != nil {
			log.Printf("backup storage multipart probe: delete after %s failed: %v", step, derr)
		}
		res.failedStep = step
		return res
	}
	if err != nil {
		return fail(probeStepCreate, err)
	}

	parts := [][]byte{make([]byte, backupstorage.MultipartMinPartSize), bytes.Repeat([]byte("p"), multipartProbeLastPart)}
	urls := make([]string, len(parts))
	for i := range parts {
		if urls[i], err = provider.UploadPartURL(ctx, key, uploadID, int32(i+1), multipartProbeURLTTL); err != nil {
			return fail(probeStepSign, err)
		}
	}
	for i, data := range parts {
		status, err := putProbePart(ctx, client, urls[i], data)
		if err != nil {
			res.status = status
			return fail(probeStepUpload, fmt.Errorf("part %d: %w", i+1, err))
		}
	}
	if _, err := provider.CompleteMultipart(ctx, key, uploadID, backupstorage.MultipartMinPartSize); err != nil {
		return fail(probeStepComplete, err)
	}
	want := backupstorage.MultipartMinPartSize + multipartProbeLastPart
	obj, err := provider.Stat(ctx, key)
	if err == nil && obj.Size != want {
		err = fmt.Errorf("completed object is %d bytes, want %d", obj.Size, want)
	}
	if err != nil {
		return fail(probeStepVerify, err)
	}
	if err := provider.Delete(ctx, key); err != nil {
		log.Printf("backup storage multipart probe: cleanup failed: %v", err)
		res.failedStep = probeStepCleanup
		return res
	}
	return res
}

// putProbePart PUTs one part and returns the HTTP status when there was an
// answer. The URL is dropped from a client error: it carries a live signature.
func putProbePart(ctx context.Context, client *http.Client, target string, data []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(data))
	if err != nil {
		return 0, probeErrWithoutURL(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, probeErrWithoutURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

func probeErrWithoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
