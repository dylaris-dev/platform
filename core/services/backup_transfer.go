package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"

	pb "dylaris-proto/node"
)

// The command fields that tell a node how to move an archive on object
// storage. Their presence is the whole signal: such a command carries no URL
// and no credentials, and the node asks Core over its control stream when the
// transfer starts.
const (
	BackupUploadMultipart   = "multipart" // backup_run "upload"
	BackupDownloadPresigned = "presigned" // backup_restore "download"
)

const (
	// backupPartSize is the fixed size of every part but the last. R2 requires
	// equal parts; 64 MiB allows archives up to 625 GiB within the 10,000-part
	// limit, and a node holds two parts in memory while it uploads.
	backupPartSize int64 = 64 << 20

	// maxPartURLsPerRequest bounds one request. The node asks as it reaches
	// parts, so a batch only has to cover the uploads it runs in parallel.
	maxPartURLsPerRequest = 8
	maxPartNumber         = 10000

	// partURLTTL is short because a URL is minted right before its part is sent.
	// S3 checks expiry when a request starts, so a slow part does not outlive it.
	partURLTTL = 15 * time.Minute
	// restoreURLTTL covers the node stopping its server before the download
	// starts; the download itself may run far longer, expiry is checked once.
	restoreURLTTL = 30 * time.Minute

	// completeUploadTimeout stays below the node's per-attempt wait (10 min) on
	// purpose: Core then always answers before the node gives up and asks a
	// second replica to complete an upload the first is still completing.
	completeUploadTimeout = 8 * time.Minute
	// transferCallTimeout bounds the other handlers' database and storage work.
	transferCallTimeout = time.Minute

	// foreignAskLogEvery bounds the log line for a node asking about a run or a
	// restore another node hosts. The node chooses what it asks, so one line
	// per request would let it fill Core's log.
	foreignAskLogEvery = time.Minute
)

// errAllowanceExceeded is the refusal for an upload that would take its owner
// past the backup storage allowance.
var errAllowanceExceeded = errTransferRefused{msg: "backup exceeds the backup storage allowance"}

// BackupTransfer answers the requests a node makes while it moves a backup
// archive to or from object storage: part URLs, completing the upload, and the
// restore URL. Core holds the credentials and the upload; the node holds only
// presigned URLs, whoever owns the node.
//
// Every handler is safe to run twice for the same request, possibly on two
// replicas at once, because the node retries on another replica.
type BackupTransfer struct {
	store    store.Store
	deps     backupstorage.Deps
	partSize int64
	// storeEnabled is config.StoreEnabled, which the backup allowance needs; see
	// BackupScheduler.storeEnabled. An upload is held to the allowance its
	// dispatch was checked against.
	storeEnabled bool

	logMu      sync.Mutex
	foreignLog map[int]foreignAskLog
}

type foreignAskLog struct {
	last       time.Time
	suppressed int
}

func NewBackupTransfer(st store.Store, deps backupstorage.Deps, storeEnabled bool) *BackupTransfer {
	return &BackupTransfer{store: st, deps: deps, partSize: backupPartSize, storeEnabled: storeEnabled}
}

// Register installs the handlers on the gRPC registry. Call before the gRPC
// server starts.
func (t *BackupTransfer) Register(reg *nodegrpc.Registry) {
	reg.HandleNodeRequest("upload_part_urls_request", t.HandleUploadPartURLs)
	reg.HandleNodeRequest("complete_upload_request", t.HandleCompleteUpload)
	reg.HandleNodeRequest("restore_url_request", t.HandleRestoreURL)
}

// errTransferRefused carries a message meant for the node. Anything else is
// logged in full and answered generically: a storage error can carry the
// backend's endpoint and bucket, and the node reports its error text into a
// run message a tenant can read.
type errTransferRefused struct{ msg string }

func (e errTransferRefused) Error() string { return e.msg }

func refused(format string, args ...interface{}) error {
	return errTransferRefused{msg: fmt.Sprintf(format, args...)}
}

// logForeignAsk logs a node asking about something another node hosts, at most
// once per foreignAskLogEvery per node, and says how many it left out.
func (t *BackupTransfer) logForeignAsk(nodeID int, format string, args ...interface{}) {
	now := time.Now()
	t.logMu.Lock()
	if t.foreignLog == nil {
		t.foreignLog = make(map[int]foreignAskLog)
	}
	entry, seen := t.foreignLog[nodeID]
	if seen && now.Sub(entry.last) < foreignAskLogEvery {
		entry.suppressed++
		t.foreignLog[nodeID] = entry
		t.logMu.Unlock()
		return
	}
	t.foreignLog[nodeID] = foreignAskLog{last: now}
	t.logMu.Unlock()
	msg := fmt.Sprintf(format, args...)
	if entry.suppressed > 0 {
		msg += fmt.Sprintf(" (and %d more such requests from this node since the last line)", entry.suppressed)
	}
	log.Print(msg)
}

func nodeMessage(err error, what string) string {
	var r errTransferRefused
	if errors.As(err, &r) {
		return r.msg
	}
	return "Core could not " + what + "; the Core log has the reason"
}

// runUploadFor loads a backup run for the node asking and opens its storage.
//
// The node is the one the stream authenticated. A run belongs to it when the
// run's job's server is hosted by it, the chain reporterMatchesServer checks for
// a result. A missing run and a foreign run get the same answer, so a node
// cannot probe which run ids exist.
func (t *BackupTransfer) runUploadFor(ctx context.Context, node nodegrpc.Node, runIDText string) (*runUpload, error) {
	runID, err := strconv.Atoi(runIDText)
	if err != nil || runID <= 0 {
		return nil, refused("invalid backup run id %q", runIDText)
	}
	notYours := refused("backup run %d is not one this node may upload", runID)
	run, err := t.store.GetBackupRun(runID)
	if err != nil || run == nil {
		return nil, notYours
	}
	job, err := t.store.GetBackupJob(run.JobID)
	if err != nil || job == nil {
		return nil, notYours
	}
	srv, err := t.store.GetServerByID(job.ServerID)
	if err != nil || srv == nil || srv.NodeID != node.ID {
		if err == nil && srv != nil {
			t.logForeignAsk(node.ID, "backup transfer: node %d asked about run %d, whose server %d is hosted by node %d - refused", node.ID, runID, srv.ID, srv.NodeID)
		}
		return nil, notYours
	}
	if run.Status != "running" {
		return nil, refused("backup run %d is no longer running (%s)", runID, run.Status)
	}
	bs, prov, err := t.openRunStorage(ctx, run, job)
	if err != nil {
		return nil, err
	}
	return &runUpload{run: run, owner: srv.OwnerID, bs: bs, prov: prov}, nil
}

// runUpload is a run a node may upload, with what its upload is checked against.
type runUpload struct {
	run   *models.BackupRun
	owner string                // the server's owner, whose allowance applies
	bs    *models.BackupStorage // where the archive goes
	prov  backupstorage.Storage
}

func (t *BackupTransfer) openRunStorage(ctx context.Context, run *models.BackupRun, job *models.BackupJob) (*models.BackupStorage, backupstorage.Storage, error) {
	bs, err := ResolveRunStorage(t.store, run, job.StorageID, BackupJobOwner(t.store, job.ServerID))
	if err != nil {
		return nil, nil, fmt.Errorf("resolve storage for run %d: %w", run.ID, err)
	}
	prov, err := backupstorage.Open(ctx, bs, t.deps)
	if err != nil {
		return nil, nil, fmt.Errorf("open storage for run %d: %w", run.ID, err)
	}
	return bs, prov, nil
}

// exceedsAllowance reports whether bytes on the upload's storage would take its
// owner past the backup storage allowance, the one BackupAllowanceExceeded
// answers for a dispatch. A running run is not part of the owner's usage (see
// store BackupBytesByOwner), so bytes is this run's whole upload so far.
func (t *BackupTransfer) exceedsAllowance(u *runUpload, bytes int64) bool {
	exceeded, used, quota := BackupAllowanceExceeded(t.store, u.owner, t.storeEnabled, u.bs)
	if !exceeded && quota == 0 {
		return false // nothing caps this storage
	}
	return used+bytes > quota
}

// HandleUploadPartURLs presigns the requested parts of a run's upload, starting
// the upload on the first request.
func (t *BackupTransfer) HandleUploadPartURLs(ctx context.Context, node nodegrpc.Node, msg *pb.NodeMessage) *pb.NodeMessage {
	req := msg.GetUploadPartUrlsRequest()
	// Detached from the stream: a node that disconnects mid-request must not
	// cancel a create between the backend and the conditional write, which is
	// the one window that could leak an upload nobody holds the id of.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transferCallTimeout)
	defer cancel()
	resp, err := t.uploadPartURLs(ctx, node, req)
	if err != nil {
		log.Printf("backup transfer: part URLs for node %d run %q: %v", node.ID, req.GetRunId(), err)
		resp = &pb.UploadPartUrlsResponse{Error: nodeMessage(err, "sign the part URLs")}
	}
	return &pb.NodeMessage{Payload: &pb.NodeMessage_UploadPartUrlsResponse{UploadPartUrlsResponse: resp}}
}

func (t *BackupTransfer) uploadPartURLs(ctx context.Context, node nodegrpc.Node, req *pb.UploadPartUrlsRequest) (*pb.UploadPartUrlsResponse, error) {
	parts := req.GetPartNumbers()
	if len(parts) == 0 || len(parts) > maxPartURLsPerRequest {
		return nil, refused("ask for 1 to %d part URLs at a time, not %d", maxPartURLsPerRequest, len(parts))
	}
	for _, p := range parts {
		if p < 1 || p > maxPartNumber {
			return nil, refused("part number %d is outside 1..%d", p, maxPartNumber)
		}
	}
	u, err := t.runUploadFor(ctx, node, req.GetRunId())
	if err != nil {
		return nil, err
	}
	run, prov := u.run, u.prov
	uploadID, partSize := run.UploadID, run.PartSize
	if uploadID == "" {
		uploadID, partSize, err = t.startUpload(ctx, run, prov)
		if err != nil {
			return nil, err
		}
	} else if err := t.checkUploadSoFar(ctx, u, uploadID, partSize, parts); err != nil {
		return nil, err
	}
	if err := t.store.TouchBackupRunTransfer(run.ID); err != nil {
		log.Printf("backup transfer: run %d: could not record upload activity: %v", run.ID, err)
	}
	resp := &pb.UploadPartUrlsResponse{UploadId: uploadID, PartSize: partSize}
	for _, p := range parts {
		url, err := prov.UploadPartURL(ctx, run.StorageKey, uploadID, p, partURLTTL)
		if err != nil {
			return nil, fmt.Errorf("presign part %d: %w", p, err)
		}
		resp.Urls = append(resp.Urls, &pb.PartUrl{PartNumber: p, Url: url})
	}
	return resp, nil
}

// checkUploadSoFar measures what the upload already holds before more part URLs
// are signed for it, and aborts it when the node is not uploading as told.
//
// A presigned UploadPart does not sign its Content-Length, so each part URL
// admits up to 5 GiB and nothing else notices before Complete. Three checks
// keep the waste to about one batch of parts:
//
//   - a part larger than the part size: the node is not cutting the archive
//     the way Core told it to, and Complete would refuse it anyway;
//   - the parts so far past the owner's allowance;
//   - a part number more than one batch ahead of the parts uploaded. A node
//     uploading as told asks for a part only once all but one of the parts
//     before it are uploaded. Without this a node could collect URLs for every
//     part number first, and the two checks above would not see any of those
//     parts before all of them were sent.
func (t *BackupTransfer) checkUploadSoFar(ctx context.Context, u *runUpload, uploadID string, partSize int64, parts []int32) error {
	usage, err := u.prov.ListMultipart(ctx, u.run.StorageKey, uploadID)
	if err != nil {
		return fmt.Errorf("list the parts uploaded so far: %w", err)
	}
	var refusal error
	switch {
	case usage.Largest > partSize:
		refusal = refused("backup run %d uploaded a part of %d bytes, larger than the part size of %d bytes", u.run.ID, usage.Largest, partSize)
	case t.exceedsAllowance(u, usage.Bytes):
		refusal = errAllowanceExceeded
	}
	if refusal != nil {
		if aerr := u.prov.AbortMultipart(ctx, u.run.StorageKey, uploadID); aerr != nil {
			log.Printf("backup transfer: run %d: could not abort the refused upload for %s: %v", u.run.ID, u.run.StorageKey, aerr)
		}
		return refusal
	}
	for _, p := range parts {
		if int(p) > usage.Parts+maxPartURLsPerRequest {
			return refused("part %d is more than %d parts ahead of the %d uploaded", p, maxPartURLsPerRequest, usage.Parts)
		}
	}
	return nil
}

// startUpload creates the run's multipart upload and stores it, or, when a
// concurrent request stored one first, aborts its own and returns the stored
// one. The store write is conditional on no upload being stored yet, so exactly
// one upload survives however many replicas get here at once.
func (t *BackupTransfer) startUpload(ctx context.Context, run *models.BackupRun, prov backupstorage.Storage) (string, int64, error) {
	created, err := prov.CreateMultipart(ctx, run.StorageKey)
	if errors.Is(err, backupstorage.ErrMultipartUnsupported) {
		return "", 0, refused("the backup target is not object storage a node can upload to")
	}
	if err != nil {
		return "", 0, fmt.Errorf("create multipart upload: %w", err)
	}
	stored, err := t.store.SetBackupRunUpload(run.ID, created, t.partSize)
	if err == nil && stored {
		return created, t.partSize, nil
	}
	if aerr := prov.AbortMultipart(ctx, run.StorageKey, created); aerr != nil {
		log.Printf("backup transfer: run %d: could not abort the surplus upload for %s: %v", run.ID, run.StorageKey, aerr)
	}
	if err != nil {
		return "", 0, fmt.Errorf("store upload id: %w", err)
	}
	again, err := t.store.GetBackupRun(run.ID)
	if err != nil || again == nil || again.UploadID == "" || again.Status != "running" {
		return "", 0, refused("backup run %d is no longer running", run.ID)
	}
	return again.UploadID, again.PartSize, nil
}

// HandleCompleteUpload completes a run's upload and returns the archive size.
func (t *BackupTransfer) HandleCompleteUpload(ctx context.Context, node nodegrpc.Node, msg *pb.NodeMessage) *pb.NodeMessage {
	req := msg.GetCompleteUploadRequest()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeUploadTimeout)
	defer cancel()
	resp, err := t.completeUpload(ctx, node, req)
	if err != nil {
		log.Printf("backup transfer: complete for node %d run %q: %v", node.ID, req.GetRunId(), err)
		resp = &pb.CompleteUploadResponse{Error: nodeMessage(err, "complete the upload")}
	}
	return &pb.NodeMessage{Payload: &pb.NodeMessage_CompleteUploadResponse{CompleteUploadResponse: resp}}
}

// completeUpload completes a run's upload within its owner's allowance and
// records the size on the run. That size, not the one the node reports
// afterwards, is what the run and the quota are given.
func (t *BackupTransfer) completeUpload(ctx context.Context, node nodegrpc.Node, req *pb.CompleteUploadRequest) (*pb.CompleteUploadResponse, error) {
	u, err := t.runUploadFor(ctx, node, req.GetRunId())
	if err != nil {
		return nil, err
	}
	run, prov := u.run, u.prov
	if run.UploadID == "" {
		return nil, refused("no upload was started for backup run %d", run.ID)
	}
	var size int64
	usage, err := prov.ListMultipart(ctx, run.StorageKey, run.UploadID)
	if err == nil {
		if t.exceedsAllowance(u, usage.Bytes) {
			if aerr := prov.AbortMultipart(ctx, run.StorageKey, run.UploadID); aerr != nil {
				log.Printf("backup transfer: run %d: could not abort the upload over the allowance for %s: %v", run.ID, run.StorageKey, aerr)
			}
			return nil, errAllowanceExceeded
		}
		size, err = prov.CompleteMultipart(ctx, run.StorageKey, run.UploadID, run.PartSize)
	}
	if err != nil {
		// A retry after a lost reply meets an upload that is already gone. The
		// object is then what says whether the first attempt completed it; the
		// key belongs to this run alone, so an object there is this run's archive.
		obj, serr := prov.Stat(ctx, run.StorageKey)
		if serr != nil {
			return nil, fmt.Errorf("complete multipart upload: %w", err)
		}
		log.Printf("backup transfer: run %d: complete failed (%v) but %s exists, so an earlier attempt completed it", run.ID, err, run.StorageKey)
		size = obj.Size
	}
	// Checked again on what was completed: parts the node sent between the
	// listing and the completion are in the object but were not in the check.
	if t.exceedsAllowance(u, size) {
		t.deleteCompleted(ctx, u, "over the allowance")
		return nil, errAllowanceExceeded
	}
	stored, err := t.store.SetBackupRunUploaded(run.ID, size)
	if err != nil {
		return nil, fmt.Errorf("record the uploaded size: %w", err)
	}
	if !stored {
		// Closed while this was completing - by a failed report, the reaper or a
		// delete - and whichever it was discarded the upload before this object
		// existed, so nothing else will remove it.
		t.deleteCompleted(ctx, u, "for a run closed during completion")
		return nil, refused("backup run %d is no longer running", run.ID)
	}
	return &pb.CompleteUploadResponse{SizeBytes: size}, nil
}

// deleteCompleted removes the archive of a refused completion, best effort: the
// refusal stands either way.
func (t *BackupTransfer) deleteCompleted(ctx context.Context, u *runUpload, why string) {
	if err := u.prov.Delete(ctx, u.run.StorageKey); err != nil {
		log.Printf("backup transfer: run %d: could not delete the archive completed %s at %s: %v", u.run.ID, why, u.run.StorageKey, err)
	}
}

// HandleRestoreURL presigns the download of the archive a restore is running.
func (t *BackupTransfer) HandleRestoreURL(ctx context.Context, node nodegrpc.Node, msg *pb.NodeMessage) *pb.NodeMessage {
	req := msg.GetRestoreUrlRequest()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transferCallTimeout)
	defer cancel()
	url, err := t.restoreURL(ctx, node, req.GetRestoreId())
	resp := &pb.RestoreUrlResponse{Url: url}
	if err != nil {
		log.Printf("backup transfer: restore URL for node %d restore %q: %v", node.ID, req.GetRestoreId(), err)
		resp = &pb.RestoreUrlResponse{Error: nodeMessage(err, "sign the restore URL")}
	}
	return &pb.NodeMessage{Payload: &pb.NodeMessage_RestoreUrlResponse{RestoreUrlResponse: resp}}
}

func (t *BackupTransfer) restoreURL(ctx context.Context, node nodegrpc.Node, restoreIDText string) (string, error) {
	restoreID, err := strconv.Atoi(restoreIDText)
	if err != nil || restoreID <= 0 {
		return "", refused("invalid restore id %q", restoreIDText)
	}
	notYours := refused("restore %d is not one this node may download", restoreID)
	restore, err := t.store.GetBackupRestore(restoreID)
	if err != nil || restore == nil {
		return "", notYours
	}
	// The restore names its target server directly, and that server is where
	// the node writes the archive.
	srv, err := t.store.GetServerByID(restore.ServerID)
	if err != nil || srv == nil || srv.NodeID != node.ID {
		if err == nil && srv != nil {
			t.logForeignAsk(node.ID, "backup transfer: node %d asked about restore %d, whose server %d is hosted by node %d - refused", node.ID, restoreID, srv.ID, srv.NodeID)
		}
		return "", notYours
	}
	// "queued" as well as "running": the node reports only the outcome of a
	// restore, so a restore it is working on is still queued in the database.
	if restore.Status != "queued" && restore.Status != "running" {
		return "", refused("restore %d is no longer running (%s)", restoreID, restore.Status)
	}
	run, err := t.store.GetBackupRun(restore.RunID)
	if err != nil || run == nil {
		return "", refused("the backup of restore %d no longer exists", restoreID)
	}
	job, err := t.store.GetBackupJob(run.JobID)
	if err != nil || job == nil {
		return "", refused("the backup of restore %d no longer exists", restoreID)
	}
	_, prov, err := t.openRunStorage(ctx, run, job)
	if err != nil {
		return "", err
	}
	url, err := prov.DownloadURL(ctx, run.StorageKey, restoreURLTTL)
	if err != nil {
		return "", fmt.Errorf("presign download: %w", err)
	}
	if url == "" {
		return "", refused("the backup target cannot hand a node a download URL")
	}
	return url, nil
}
