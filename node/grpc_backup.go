package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	pb "dylaris-proto/node"
)

// backupDirName is the hidden directory inside each server folder where
// node-local backup archives are kept. The `.dylaris-` prefix makes the
// existing file-browser write guard (validateBeamPath in beam_server.go)
// reject any attempt to write through it; this file only ever resolves
// paths *into* that directory for the dedicated backup RPCs.
//
// Single source of truth for the directory name across the package —
// grpc_backup.go, backup_worker.go and backup_restore.go all reference it.
const backupDirName = ".dylaris-backups"

// resolveBackupDir returns the absolute path of the .dylaris-backups
// directory for the given server UUID. It does NOT create it: only the backup
// writer does. Created here, Core's archive purge right after a server delete
// brought the deleted server's directory back on disk. A missing directory is
// an empty list, a zero usage and a 404 to its callers.
func (h *StreamHandler) resolveBackupDir(serverUUID string) (string, error) {
	if serverUUID == "" {
		return "", fmt.Errorf("server_uuid required")
	}
	if h.storageMgr == nil {
		return "", fmt.Errorf("storage manager not initialised")
	}
	serverDir := h.storageMgr.GetServerDir(serverUUID)
	if serverDir == "" {
		return "", fmt.Errorf("invalid server_uuid")
	}
	return filepath.Join(serverDir, backupDirName), nil
}

// resolveBackupKey accepts an archive filename (NOT a path), validates it
// doesn't escape .dylaris-backups/ via traversal, and returns the absolute
// on-disk path. Keys are intentionally flat — every node-local archive
// lands directly in .dylaris-backups/ — so we reject any input containing
// slashes or "..".
func (h *StreamHandler) resolveBackupKey(serverUUID, key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("backup key required")
	}
	if strings.ContainsAny(key, `/\`) || strings.Contains(key, "..") {
		return "", fmt.Errorf("backup key must be a flat filename")
	}
	dir, err := h.resolveBackupDir(serverUUID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, key), nil
}

// openArchive opens a node-local archive and refuses anything that is not a
// plain file.
//
// .dylaris-backups sits INSIDE the server directory, and that directory is
// bind-mounted into the tenant's own Minecraft container at /data. The file API
// refuses to write through a ".dylaris-" path, but a plugin running in that
// container does not go through the file API: "ln -s /whatever
// /data/.dylaris-backups/x" is one line. Every function here then followed it,
// because Stat and Open both do - so the archive download RPC would read the
// link's target and stream it out through the panel's own download button, with
// the node's process credentials. The node writes these archives itself and
// never as a link, so a link is never something to resolve carefully; it is
// something that has no business being here.
//
// Checked on the OPEN file, through a Root at the server directory: an Lstat
// of the path and an Open of it later are two lookups, and a link swapped in
// between (or .dylaris-backups itself replaced by a link) was followed by the
// open. The Root refuses any link out of the server directory.
func openArchive(archivePath string) (*os.File, error) {
	serverDir := filepath.Dir(filepath.Dir(archivePath))
	root, err := os.OpenRoot(serverDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := openRegularIn(root, filepath.Join(backupDirName, filepath.Base(archivePath)))
	if err != nil {
		return nil, fmt.Errorf("not a backup archive: %w", err)
	}
	return f, nil
}

// handleBackupList walks .dylaris-backups/ and returns one BackupObject
// per archive. Empty when the directory doesn't exist or is empty.
func (h *StreamHandler) handleBackupList(reqID, serverUUID string) *pb.NodeMessage {
	dir, err := h.resolveBackupDir(serverUUID)
	if err != nil {
		return errorMsg(reqID, 500, err.Error())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return &pb.NodeMessage{
				RequestId: reqID,
				Payload: &pb.NodeMessage_BackupListResp{
					BackupListResp: &pb.BackupListResp{},
				},
			}
		}
		return errorMsg(reqID, 500, "read backup dir: "+err.Error())
	}
	objs := make([]*pb.BackupObject, 0, len(entries))
	for _, e := range entries {
		// Info() is Lstat-based, so a planted symlink shows as one here and is
		// dropped rather than listed as a downloadable archive. See openArchive.
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		// An archive still being written is no backup yet. Listed, Core's
		// orphan scan would see a key no run names and delete it mid-write.
		if strings.HasSuffix(e.Name(), partialBackupSuffix) {
			continue
		}
		objs = append(objs, &pb.BackupObject{
			Key:          e.Name(),
			Size:         info.Size(),
			ModifiedUnix: info.ModTime().Unix(),
		})
	}
	return &pb.NodeMessage{
		RequestId: reqID,
		Payload: &pb.NodeMessage_BackupListResp{
			BackupListResp: &pb.BackupListResp{Objects: objs},
		},
	}
}

// handleBackupDelete removes one archive. Idempotent — a missing file
// returns a synthetic 404 error so Core can choose to log-and-continue
// during retention sweeps.
func (h *StreamHandler) handleBackupDelete(reqID, serverUUID string, req *pb.BackupDeleteReq) *pb.NodeMessage {
	path, err := h.resolveBackupKey(serverUUID, req.Key)
	if err != nil {
		return errorMsg(reqID, 400, err.Error())
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return errorMsg(reqID, 404, "backup not found")
		}
		return errorMsg(reqID, 500, "delete: "+err.Error())
	}
	return &pb.NodeMessage{
		RequestId: reqID,
		Payload:   &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "deleted"}},
	}
}

// handleBackupUsage sums archive sizes in .dylaris-backups/ so the Overview
// tab can render a separate "Backups" usage bar when the server is on
// node-local storage. Cheap enough to compute on each call — backup folders
// rarely exceed a few entries per server.
func (h *StreamHandler) handleBackupUsage(reqID, serverUUID string) *pb.NodeMessage {
	dir, err := h.resolveBackupDir(serverUUID)
	if err != nil {
		return errorMsg(reqID, 500, err.Error())
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return errorMsg(reqID, 500, "read backup dir: "+err.Error())
	}
	var total int64
	var count int32
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		// Not a backup, as in the list. A run a node restart ended leaves its
		// .partial until the boot sweep, and counted here it held the quota
		// Core refuses new runs against, with no archive anyone could delete.
		if strings.HasSuffix(e.Name(), partialBackupSuffix) {
			continue
		}
		total += info.Size()
		count++
	}
	return &pb.NodeMessage{
		RequestId: reqID,
		Payload: &pb.NodeMessage_BackupUsageResp{
			BackupUsageResp: &pb.BackupUsageResp{
				UsedBytes: total,
				Count:     count,
			},
		},
	}
}

// streamBackupArchive streams one archive over a NodeMessage stream. It
// mirrors streamFile in grpc_handler.go but pins the source to
// .dylaris-backups/ so a buggy/malicious Core can't repurpose this RPC to
// fetch arbitrary server files (those have their own ReadReq).
func (h *StreamHandler) streamBackupArchive(reqID, serverUUID string, req *pb.BackupOpenReq, sendFn func(*pb.NodeMessage) error) {
	path, err := h.resolveBackupKey(serverUUID, req.Key)
	if err != nil {
		sendFn(errorMsg(reqID, 400, err.Error()))
		return
	}
	f, err := openArchive(path)
	if err != nil {
		sendFn(errorMsg(reqID, 404, "backup not found"))
		return
	}
	defer f.Close()

	// Leading metadata TransferDone — same protocol StreamFile uses, so
	// Core's reader treats this identically.
	if err := sendFn(&pb.NodeMessage{
		RequestId: reqID,
		Payload: &pb.NodeMessage_TransferDone{
			TransferDone: &pb.TransferDone{
				Filename:   filepath.Base(path),
				TotalBytes: 0,
			},
		},
	}); err != nil {
		return
	}

	buf := make([]byte, chunkSize)
	var offset int64
	for {
		n, rErr := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := sendFn(&pb.NodeMessage{
				RequestId: reqID,
				Payload: &pb.NodeMessage_Chunk{
					Chunk: &pb.DataChunk{Data: chunk, Offset: offset},
				},
			}); err != nil {
				return
			}
			offset += int64(n)
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			sendFn(errorMsg(reqID, 500, "read backup: "+rErr.Error()))
			return
		}
	}

	// Final TransferDone — no Filename, TotalBytes > 0 for non-empty files
	// (and == 0 for empty, but a zero-byte archive shouldn't ever exist).
	sendFn(&pb.NodeMessage{
		RequestId: reqID,
		Payload: &pb.NodeMessage_TransferDone{
			TransferDone: &pb.TransferDone{TotalBytes: offset},
		},
	})
}
