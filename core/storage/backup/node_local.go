package backup

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	nodegrpc "dylaris-core/grpc"

	pb "dylaris-proto/node"

	"github.com/google/uuid"
)

// NodeLocalStorage represents the "node-local" backup mode. Archives live
// at <server-dir>/.dylaris-backups/<key>.tar.gz on whichever Node hosts the
// container. The provider itself owns no bytes — every operation forwards
// to the Node over the existing gRPC mesh.
//
// Storage keys are encoded as "<serverUUID>/<backupID>.tar.gz" so the
// provider can derive both the target Node (via NodeStore lookup) and the
// filename to use inside the .dylaris-backups/ directory without any extra
// metadata. This matches how the backup handler already builds keys for
// other providers; the leading "backups/" prefix used by s3/shared is
// stripped before dispatch.
type NodeLocalStorage struct {
	registry  *nodegrpc.Registry
	nodeStore NodeStore
}

// NewNodeLocal builds a node-local provider. Both deps must be non-nil —
// the factory enforces this before constructing one.
func NewNodeLocal(registry *nodegrpc.Registry, nodeStore NodeStore) *NodeLocalStorage {
	return &NodeLocalStorage{registry: registry, nodeStore: nodeStore}
}

func (n *NodeLocalStorage) Provider() string { return "node-local" }

// resolveKey splits a storage key of the form
//
//	[backups/]<serverUUID>/<backupID>.tar.gz
//
// into (serverUUID, backupID.tar.gz). The optional "backups/" prefix is
// tolerated so handler code that builds keys identically for every
// provider doesn't have to special-case node-local.
func resolveNodeLocalKey(key string) (serverUUID, archiveName string, err error) {
	clean := strings.TrimPrefix(key, "backups/")
	clean = strings.TrimPrefix(clean, "/")
	parts := strings.SplitN(clean, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("node-local key must encode <serverUUID>/<archive>, got %q", key)
	}
	// The on-disk filename collapses any nested "job-N/" path component
	// so every archive lands directly in .dylaris-backups/ — simplifies
	// listing and quota accounting. The handler-side StorageKey still
	// retains the original prefix for DB display.
	tail := strings.Split(parts[1], "/")
	return parts[0], tail[len(tail)-1], nil
}

// nodeIDForServer looks up which Node currently hosts the given server
// UUID. Returns an error if the server is unknown.
func (n *NodeLocalStorage) nodeIDForServer(serverUUID string) (int, error) {
	srv, err := n.nodeStore.GetServerByUUID(serverUUID)
	if err != nil {
		return 0, fmt.Errorf("server %s not found: %w", serverUUID, err)
	}
	return srv.NodeID, nil
}

// Put is a no-op for node-local. The Node's backup_worker writes the
// archive directly to its local disk after pulling the BackupRunCommand
// from Redis, so Core never streams bytes. This method is only called by
// the admin TestStorage probe, which we satisfy with a synthetic OK so
// admins still see "Connection OK" when they hit the test button.
func (n *NodeLocalStorage) Put(_ context.Context, _ string, r io.Reader, _ int64) error {
	if r != nil {
		// Drain so the test handler can release whatever buffer it passed.
		io.Copy(io.Discard, r)
	}
	return nil
}

// Get streams an archive from the owning Node over the gRPC mesh. The
// returned reader pulls chunks lazily — caller MUST close to release the
// pending request slot.
func (n *NodeLocalStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	serverUUID, archive, err := resolveNodeLocalKey(key)
	if err != nil {
		return nil, err
	}
	nodeID, err := n.nodeIDForServer(serverUUID)
	if err != nil {
		return nil, err
	}

	reqID := uuid.NewString()
	msg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload: &pb.NodeMessage_BackupOpenReq{
			BackupOpenReq: &pb.BackupOpenReq{Key: archive},
		},
	}

	ch, err := n.registry.SendRequestStreaming(nodeID, msg)
	if err != nil {
		return nil, fmt.Errorf("dispatch backup-open to node %d: %w", nodeID, err)
	}

	return &nodeLocalReader{
		ctx:      ctx,
		ch:       ch,
		registry: n.registry,
		nodeID:   nodeID,
		reqID:    reqID,
	}, nil
}

// Delete asks the owning Node to remove the archive from its
// .dylaris-backups/ directory. Idempotent: a missing key returns nil so
// retention sweeps can be safely re-run.
func (n *NodeLocalStorage) Delete(ctx context.Context, key string) error {
	serverUUID, archive, err := resolveNodeLocalKey(key)
	if err != nil {
		return err
	}
	nodeID, err := n.nodeIDForServer(serverUUID)
	if err != nil {
		return err
	}

	reqID := uuid.NewString()
	msg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload: &pb.NodeMessage_BackupDeleteReq{
			BackupDeleteReq: &pb.BackupDeleteReq{Key: archive},
		},
	}

	resp, err := n.registry.SendRequest(nodeID, msg, 15*time.Second)
	if err != nil {
		return err
	}
	if e := resp.GetError(); e != nil && e.Code != 404 {
		return fmt.Errorf("node delete failed: %s", e.Message)
	}
	return nil
}

// List returns the archives kept under the prefix's serverUUID. The
// `prefix` value is expected to be "<serverUUID>" or "<serverUUID>/..."
// — only the first segment is honored. Anything else is treated as an
// empty list, matching how s3/shared behave on missing prefixes.
func (n *NodeLocalStorage) List(ctx context.Context, prefix string) ([]Object, error) {
	clean := strings.TrimPrefix(prefix, "backups/")
	clean = strings.TrimPrefix(clean, "/")
	parts := strings.SplitN(clean, "/", 2)
	if parts[0] == "" {
		return nil, nil
	}
	serverUUID := parts[0]

	nodeID, err := n.nodeIDForServer(serverUUID)
	if err != nil {
		return nil, err
	}

	reqID := uuid.NewString()
	msg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload: &pb.NodeMessage_BackupListReq{
			BackupListReq: &pb.BackupListReq{},
		},
	}

	resp, err := n.registry.SendRequest(nodeID, msg, 15*time.Second)
	if err != nil {
		return nil, err
	}
	if e := resp.GetError(); e != nil {
		return nil, fmt.Errorf("node list failed: %s", e.Message)
	}
	list := resp.GetBackupListResp()
	if list == nil {
		return nil, nil
	}
	out := make([]Object, 0, len(list.Objects))
	for _, o := range list.Objects {
		out = append(out, Object{
			Key:          serverUUID + "/" + o.Key,
			Size:         o.Size,
			LastModified: time.Unix(o.ModifiedUnix, 0),
		})
	}
	return out, nil
}

// Stat returns size/mtime for a single archive. Implemented on top of
// List since the per-server archive count is small and the gRPC round-
// trip dominates either way.
func (n *NodeLocalStorage) Stat(ctx context.Context, key string) (Object, error) {
	serverUUID, archive, err := resolveNodeLocalKey(key)
	if err != nil {
		return Object{}, err
	}
	objs, err := n.List(ctx, serverUUID)
	if err != nil {
		return Object{}, err
	}
	for _, o := range objs {
		if strings.HasSuffix(o.Key, "/"+archive) {
			return o, nil
		}
	}
	// Wrapped so errors.Is(err, fs.ErrNotExist) holds, which is what the
	// Storage interface promises and what the backup reaper decides on: it has
	// to tell "the node never wrote this archive" from "the node could not be
	// asked". Returning a bare fmt.Errorf here made every abandoned run on a
	// node-local job report the second when it meant the first.
	return Object{}, fmt.Errorf("backup %s not found: %w", key, fs.ErrNotExist)
}

// DownloadURL returns "" so the Core backup handler streams the archive
// through itself (which then proxies to the owning Node via Get).
// Matches the local/shared providers' behavior.
func (n *NodeLocalStorage) DownloadURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", nil
}

func (n *NodeLocalStorage) UploadURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", nil
}

func (*NodeLocalStorage) CreateMultipart(context.Context, string) (string, error) {
	return "", ErrMultipartUnsupported
}

func (*NodeLocalStorage) UploadPartURL(context.Context, string, string, int32, time.Duration) (string, error) {
	return "", ErrMultipartUnsupported
}

func (*NodeLocalStorage) CompleteMultipart(context.Context, string, string, int64) (int64, error) {
	return 0, ErrMultipartUnsupported
}

func (*NodeLocalStorage) AbortMultipart(context.Context, string, string) error {
	return ErrMultipartUnsupported
}

func (*NodeLocalStorage) ListMultipart(context.Context, string, string) (MultipartUsage, error) {
	return MultipartUsage{}, ErrMultipartUnsupported
}

// nodeLocalReader adapts the gRPC streaming channel into an io.ReadCloser.
// Chunks arrive in order; we keep one in-memory buffer and copy out of it
// as Read is called. The Node sends a final TransferDone with no Filename
// to mark EOF — the registry closes the channel at that point, which we
// translate into io.EOF for the caller.
type nodeLocalReader struct {
	ctx      context.Context
	ch       <-chan *pb.NodeMessage
	registry *nodegrpc.Registry
	nodeID   int
	reqID    string

	buf    []byte // unread bytes from the most recent chunk
	closed bool
	err    error
}

func (r *nodeLocalReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	for len(r.buf) == 0 {
		select {
		case <-r.ctx.Done():
			r.err = r.ctx.Err()
			return 0, r.err
		case msg, ok := <-r.ch:
			if !ok {
				r.err = io.EOF
				return 0, r.err
			}
			if e := msg.GetError(); e != nil {
				r.err = fmt.Errorf("node backup-open error: %s", e.Message)
				return 0, r.err
			}
			if chunk := msg.GetChunk(); chunk != nil && len(chunk.Data) > 0 {
				r.buf = chunk.Data
				continue
			}
			if done := msg.GetTransferDone(); done != nil {
				// Two TransferDones can arrive — a leading metadata one
				// (Filename set, TotalBytes==0) and a final one. We don't
				// rely on either: the channel close (after the final one)
				// is the EOF signal. Just keep looping.
				continue
			}
			// Unknown payload — ignore.
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *nodeLocalReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	// Drain the channel so the routing layer's pending map isn't leaked.
	// SendRequestStreaming already arranged that the registry will close
	// the channel when TransferDone arrives, but a caller that gives up
	// mid-download needs explicit cleanup here.
	r.registry.CleanupRequest(r.nodeID, r.reqID)
	return nil
}
