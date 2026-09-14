package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
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
)

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
}

func NewBackupTransfer(st store.Store, deps backupstorage.Deps) *BackupTransfer {
	return &BackupTransfer{store: st, deps: deps, partSize: backupPartSize}
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
func (t *BackupTransfer) runUploadFor(ctx context.Context, node nodegrpc.Node, runIDText string) (*models.BackupRun, backupstorage.Storage, error) {
	runID, err := strconv.Atoi(runIDText)
	if err != nil || runID <= 0 {
		return nil, nil, refused("invalid backup run id %q", runIDText)
	}
	notYours := refused("backup run %d is not one this node may upload", runID)
	run, err := t.store.GetBackupRun(runID)
	if err != nil || run == nil {
		return nil, nil, notYours
	}
	job, err := t.store.GetBackupJob(run.JobID)
	if err != nil || job == nil {
		return nil, nil, notYours
	}
	srv, err := t.store.GetServerByID(job.ServerID)
	if err != nil || srv == nil || srv.NodeID != node.ID {
		if err == nil && srv != nil {
			log.Printf("backup transfer: node %d asked about run %d, whose server %d is hosted by node %d - refused", node.ID, runID, srv.ID, srv.NodeID)
		}
		return nil, nil, notYours
	}
	if run.Status != "running" {
		return nil, nil, refused("backup run %d is no longer running (%s)", runID, run.Status)
	}
	prov, err := t.openRunStorage(ctx, run, job)
	if err != nil {
		return nil, nil, err
	}
	return run, prov, nil
}

func (t *BackupTransfer) openRunStorage(ctx context.Context, run *models.BackupRun, job *models.BackupJob) (backupstorage.Storage, error) {
	bs, err := ResolveRunStorage(t.store, run, job.StorageID, BackupJobOwner(t.store, job.ServerID))
	if err != nil {
		return nil, fmt.Errorf("resolve storage for run %d: %w", run.ID, err)
	}
	prov, err := backupstorage.Open(ctx, bs, t.deps)
	if err != nil {
		return nil, fmt.Errorf("open storage for run %d: %w", run.ID, err)
	}
	return prov, nil
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
	run, prov, err := t.runUploadFor(ctx, node, req.GetRunId())
	if err != nil {
		return nil, err
	}
	uploadID, partSize := run.UploadID, run.PartSize
	if uploadID == "" {
		uploadID, partSize, err = t.startUpload(ctx, run, prov)
		if err != nil {
			return nil, err
		}
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

func (t *BackupTransfer) completeUpload(ctx context.Context, node nodegrpc.Node, req *pb.CompleteUploadRequest) (*pb.CompleteUploadResponse, error) {
	run, prov, err := t.runUploadFor(ctx, node, req.GetRunId())
	if err != nil {
		return nil, err
	}
	if run.UploadID == "" {
		return nil, refused("no upload was started for backup run %d", run.ID)
	}
	size, err := prov.CompleteMultipart(ctx, run.StorageKey, run.UploadID, run.PartSize)
	if err == nil {
		return &pb.CompleteUploadResponse{SizeBytes: size}, nil
	}
	// A retry after a lost reply meets an upload that is already gone. The
	// object is then what says whether the first attempt completed it; the key
	// belongs to this run alone, so an object there is this run's archive.
	if obj, serr := prov.Stat(ctx, run.StorageKey); serr == nil {
		log.Printf("backup transfer: run %d: complete failed (%v) but %s exists, so an earlier attempt completed it", run.ID, err, run.StorageKey)
		return &pb.CompleteUploadResponse{SizeBytes: obj.Size}, nil
	}
	return nil, fmt.Errorf("complete multipart upload: %w", err)
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
			log.Printf("backup transfer: node %d asked about restore %d, whose server %d is hosted by node %d - refused", node.ID, restoreID, srv.ID, srv.NodeID)
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
	prov, err := t.openRunStorage(ctx, run, job)
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
