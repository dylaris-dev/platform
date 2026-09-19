package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	pb "dylaris-proto/node"
)

const chunkSize = 64 * 1024 // 64KB

// StreamHandler processes file operation requests from Core via gRPC.
// It operates on the Node's local filesystem.
type StreamHandler struct {
	baseDir    string          // Node's working directory (legacy fallback)
	storageMgr *StorageManager // Multi-path storage manager
}

func NewStreamHandler(storageMgr *StorageManager) *StreamHandler {
	baseDir, _ := os.Getwd()
	return &StreamHandler{baseDir: baseDir, storageMgr: storageMgr}
}

// Handle processes a NodeMessage and returns one or more response messages.
// ReadReq is handled separately via HandleStreaming for memory-efficient streaming.
func (h *StreamHandler) Handle(msg *pb.NodeMessage) []*pb.NodeMessage {
	switch p := msg.Payload.(type) {
	case *pb.NodeMessage_ListReq:
		return []*pb.NodeMessage{h.handleList(msg.RequestId, msg.ServerUuid, p.ListReq)}
	case *pb.NodeMessage_WriteReq:
		return []*pb.NodeMessage{h.handleWrite(msg.RequestId, msg.ServerUuid, p.WriteReq)}
	case *pb.NodeMessage_CreateReq:
		return []*pb.NodeMessage{h.handleCreate(msg.RequestId, msg.ServerUuid, p.CreateReq)}
	case *pb.NodeMessage_DeleteReq:
		return []*pb.NodeMessage{h.handleDelete(msg.RequestId, msg.ServerUuid, p.DeleteReq)}
	case *pb.NodeMessage_RenameReq:
		return []*pb.NodeMessage{h.handleRename(msg.RequestId, msg.ServerUuid, p.RenameReq)}
	case *pb.NodeMessage_CopyReq:
		return []*pb.NodeMessage{h.handleCopy(msg.RequestId, msg.ServerUuid, p.CopyReq)}
	case *pb.NodeMessage_InspectOrphanReq:
		return []*pb.NodeMessage{h.handleInspectOrphan(msg.RequestId, msg.ServerUuid)}
	case *pb.NodeMessage_BackupListReq:
		return []*pb.NodeMessage{h.handleBackupList(msg.RequestId, msg.ServerUuid)}
	case *pb.NodeMessage_BackupDeleteReq:
		return []*pb.NodeMessage{h.handleBackupDelete(msg.RequestId, msg.ServerUuid, p.BackupDeleteReq)}
	case *pb.NodeMessage_BackupUsageReq:
		return []*pb.NodeMessage{h.handleBackupUsage(msg.RequestId, msg.ServerUuid)}
	case *pb.NodeMessage_RconExecReq:
		return []*pb.NodeMessage{h.handleRconExec(msg.RequestId, msg.ServerUuid, p.RconExecReq)}
	case *pb.NodeMessage_HashFilesReq:
		return []*pb.NodeMessage{h.handleHashFiles(msg.RequestId, msg.ServerUuid, p.HashFilesReq)}
	default:
		return []*pb.NodeMessage{errorMsg(msg.RequestId, 400, "unknown request type")}
	}
}

// HandleStreaming processes read operations by streaming chunks via sendFn callback.
// This avoids buffering entire files/zips in memory (~128KB constant RAM per download).
func (h *StreamHandler) HandleStreaming(msg *pb.NodeMessage, sendFn func(*pb.NodeMessage) error) {
	// SelectiveReadReq: zip selected files/folders
	if selReq := msg.GetSelectiveReadReq(); selReq != nil {
		h.streamSelectiveZip(msg.RequestId, msg.ServerUuid, selReq, sendFn)
		return
	}

	// BackupOpenReq: stream one .dylaris-backups/<key>.tar.gz to Core. Path
	// resolution is the same as a regular file read but pinned to the hidden
	// backup directory so Core can't accidentally fetch arbitrary files
	// using the backup-open RPC.
	if openReq := msg.GetBackupOpenReq(); openReq != nil {
		h.streamBackupArchive(msg.RequestId, msg.ServerUuid, openReq, sendFn)
		return
	}

	readReq := msg.GetReadReq()
	if readReq == nil {
		sendFn(errorMsg(msg.RequestId, 400, "HandleStreaming only supports ReadReq/SelectiveReadReq/BackupOpenReq"))
		return
	}

	filePath, err := h.validatePath(readReq.Path, msg.ServerUuid)
	if err != nil {
		sendFn(errorMsg(msg.RequestId, 403, err.Error()))
		return
	}

	stat, err := os.Stat(filePath)
	if err != nil {
		sendFn(errorMsg(msg.RequestId, 404, "file not found"))
		return
	}

	if stat.IsDir() && readReq.ZipIfDir {
		h.streamDirAsZip(msg.RequestId, msg.ServerUuid, filePath, sendFn)
		return
	}

	if stat.IsDir() {
		sendFn(errorMsg(msg.RequestId, 400, "path is a directory, set zip_if_dir=true"))
		return
	}

	h.streamFile(msg.RequestId, filePath, sendFn)
}

// resolveWithinDir joins reqPath under dataPath and guarantees the result
// stays inside dataPath. It is the single source of truth for the
// path-traversal guard shared by the gRPC file handler (validatePath), the
// selective-zip download path, and the Beam server (validateBeamPathOp).
//
// Containment is checked with a trailing separator so a sibling directory that
// merely shares the prefix (e.g. dataPath+"-evil") cannot pass — without it,
// reqPath "../<base>-evil/secret" would resolve to a sibling and slip through.
func resolveWithinDir(dataPath, reqPath string) (string, error) {
	cleanPath := filepath.Clean(filepath.Join(dataPath, reqPath))
	cleanData := filepath.Clean(dataPath)
	if cleanPath != cleanData && !strings.HasPrefix(cleanPath, cleanData+string(os.PathSeparator)) {
		return "", fmt.Errorf("access denied: path traversal")
	}
	if !linkStaysWithin(cleanData, cleanPath) {
		return "", fmt.Errorf("access denied: path leaves the server directory")
	}
	return cleanPath, nil
}

// linkStaysWithin reports whether the filesystem agrees that cleanPath - which
// already passed the lexical check above - still lands inside lexRoot once
// every symlink on the way to it is followed.
//
// The lexical check cannot see this. It cleans and prefix-checks a string and
// never asks the filesystem, so it accepted a planted link and every operation
// then reached the link's TARGET: a plain download returned it, a copy
// materialised it as a real file inside the tenant's own directory. A tenant
// plants one from inside their own Minecraft container (the server directory is
// bind-mounted into it), and the link text is resolved on the NODE's side, so it
// can name a path that exists only there - .node_secret, another tenant's server
// directory, /proc/self/environ. The archive walkers had their own guard for
// exactly this (zipEntryInfo); the boundary itself did not.
//
// One rule, no per-operation exceptions. Deleting or renaming a link never
// touches its target and would be safe to allow, but a second, laxer variant is
// a second thing to reach for by mistake, and the cost of the strict rule is
// small: the containing directory can still be removed, which takes the link
// with it.
func linkStaysWithin(lexRoot, cleanPath string) bool {
	return resolvesInside(lexRoot, resolveZipRoot(lexRoot), cleanPath)
}

// resolvesInside is linkStaysWithin's recursion. lexRoot bounds the walk up the
// LEXICAL path; resolvedRoot is what a resolved path is measured against. They
// differ whenever the storage path itself is a symlink, and comparing a
// resolved path to the lexical root would then reject every contained link.
func resolvesInside(lexRoot, resolvedRoot, path string) bool {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return withinRoot(resolvedRoot, resolved)
	}
	// Not resolvable, but Lstat can still see it: a DANGLING link, which is a
	// trap for whatever creates the file next - an open with O_CREAT follows it
	// and creates the target.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	// The path does not exist yet (an upload, a new directory). It is safe
	// exactly when the deepest ancestor that DOES exist resolves inside.
	parent := filepath.Dir(path)
	if parent == path || !withinRoot(lexRoot, parent) {
		return true // walked past the root without finding anything to follow
	}
	return resolvesInside(lexRoot, resolvedRoot, parent)
}

// withinRoot reports whether abs is root itself or lies underneath it. Same
// trailing-separator containment rule as resolveWithinDir, but for a path that
// is already absolute rather than one being joined onto a base.
func withinRoot(root, abs string) bool {
	cleanRoot := filepath.Clean(root)
	cleanAbs := filepath.Clean(abs)
	return cleanAbs == cleanRoot || strings.HasPrefix(cleanAbs, cleanRoot+string(os.PathSeparator))
}

// resolveZipRoot prepares the symlink boundary for zipEntryInfo. Resolving the
// root once matters: a STORAGE_PATHS entry that is itself a symlink would
// otherwise make every contained link look like an escape, because zipEntryInfo
// compares against an EvalSymlinks'd target.
func resolveZipRoot(root string) string {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		return r
	}
	return filepath.Clean(root)
}

// zipEntryInfo decides whether a walked path belongs in an archive and which
// FileInfo describes it. Despite the name it is not zip-specific: the backup
// tar builder in backup_worker.go walks the same trees and calls it too.
//
// This closes a symlink hole shared by every archive path here: filepath.Walk
// reports links via Lstat, but os.Open FOLLOWS them, so a link planted inside a
// tenant's server directory would have its target read and written into the
// archive - an arbitrary-file-read out of that directory, dressed up as a folder
// download. A tenant can plant one: the server directory is bind-mounted into
// their Minecraft container and reachable over SFTP.
//
// Contained links are still archived, so this is not a blanket "drop all
// symlinks" that would break legitimate layouts.
func zipEntryInfo(resolvedRoot, path string, info os.FileInfo) (os.FileInfo, bool) {
	if info.Mode()&os.ModeSymlink == 0 {
		return info, true
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, false // dangling: nothing to archive
	}
	if !withinRoot(resolvedRoot, resolved) {
		return nil, false // escapes the server directory
	}
	target, err := os.Stat(resolved)
	// Walk does not descend into symlinked directories, so one added as a file
	// would fail on io.Copy. Skip it rather than write a half-entry.
	if err != nil || target.IsDir() {
		return nil, false
	}
	return target, true
}

// serverDir returns the server's data directory, the root every path guard in
// this file is measured against.
func (h *StreamHandler) serverDir(serverUUID string) string {
	if h.storageMgr != nil {
		return h.storageMgr.GetServerDir(serverUUID)
	}
	return filepath.Join(h.baseDir, "dylaris_data", "servers", serverUUID)
}

// validatePath ensures the path stays within the server's data directory.
func (h *StreamHandler) validatePath(reqPath, serverUUID string) (string, error) {
	if serverUUID == "" {
		return "", fmt.Errorf("server_uuid required")
	}

	// Use StorageManager for dynamic path resolution if available
	var dataPath string
	if h.storageMgr != nil {
		dataPath = h.storageMgr.GetServerDir(serverUUID)
	} else {
		dataPath = filepath.Join(h.baseDir, "dylaris_data", "servers", serverUUID)
	}

	return resolveWithinDir(dataPath, reqPath)
}

func (h *StreamHandler) handleList(reqID, serverUUID string, req *pb.ListFilesReq) *pb.NodeMessage {
	dirPath, err := h.validatePath(req.Path, serverUUID)
	if err != nil {
		return errorMsg(reqID, 403, err.Error())
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Return empty list for non-existent directories
			return &pb.NodeMessage{
				RequestId: reqID,
				Payload: &pb.NodeMessage_ListResp{
					ListResp: &pb.ListFilesResp{Files: []*pb.FileInfo{}},
				},
			}
		}
		return errorMsg(reqID, 500, fmt.Sprintf("read dir: %v", err))
	}

	// Pre-build the file list and collect directory entries for concurrent size calculation
	type dirEntry struct {
		info *pb.FileInfo
		path string
	}
	files := make([]*pb.FileInfo, 0, len(entries))
	var dirs []dirEntry
	for _, e := range entries {
		if isProtectedFile(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		f := &pb.FileInfo{
			Name:  e.Name(),
			IsDir: e.IsDir(),
			Size:  fi.Size(),
		}
		if e.IsDir() {
			dirs = append(dirs, dirEntry{info: f, path: filepath.Join(dirPath, e.Name())})
		}
		files = append(files, f)
	}

	// Calculate directory sizes concurrently
	if len(dirs) > 0 {
		var wg sync.WaitGroup
		for _, de := range dirs {
			wg.Add(1)
			go func(f *pb.FileInfo, p string) {
				defer wg.Done()
				f.Size = dirSize(p)
			}(de.info, de.path)
		}
		wg.Wait()
	}

	return &pb.NodeMessage{
		RequestId: reqID,
		Payload: &pb.NodeMessage_ListResp{
			ListResp: &pb.ListFilesResp{Files: files},
		},
	}
}

// streamFile streams a single file in 64KB chunks via sendFn.
// Sends metadata TransferDone first (TotalBytes=0), then chunks, then final TransferDone.
func (h *StreamHandler) streamFile(reqID, filePath string, sendFn func(*pb.NodeMessage) error) {
	filename := filepath.Base(filePath)

	// Send metadata first (filename for Content-Disposition header)
	if err := sendFn(&pb.NodeMessage{
		RequestId: reqID,
		Payload: &pb.NodeMessage_TransferDone{
			TransferDone: &pb.TransferDone{Filename: filename, TotalBytes: 0},
		},
	}); err != nil {
		return
	}

	f, err := os.Open(filePath)
	if err != nil {
		sendFn(errorMsg(reqID, 500, fmt.Sprintf("open: %v", err)))
		return
	}
	defer f.Close()

	buf := make([]byte, chunkSize)
	var offset int64

	for {
		n, readErr := f.Read(buf)
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
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			sendFn(errorMsg(reqID, 500, fmt.Sprintf("read: %v", readErr)))
			return
		}
	}

	// Final TransferDone — no Filename, so core can distinguish it from the
	// metadata TransferDone even when TotalBytes==0 (empty files).
	sendFn(&pb.NodeMessage{
		RequestId: reqID,
		Payload: &pb.NodeMessage_TransferDone{
			TransferDone: &pb.TransferDone{TotalBytes: offset},
		},
	})
}

// streamDirAsZip creates a zip of the directory using io.Pipe and streams chunks
// as they are produced. Constant ~128KB RAM usage regardless of directory size.
func (h *StreamHandler) streamDirAsZip(reqID, serverUUID, dirPath string, sendFn func(*pb.NodeMessage) error) {
	resolvedRoot := resolveZipRoot(h.serverDir(serverUUID))
	filename := filepath.Base(dirPath) + ".zip"

	// Send metadata first (filename for Content-Disposition header)
	if err := sendFn(&pb.NodeMessage{
		RequestId: reqID,
		Payload: &pb.NodeMessage_TransferDone{
			TransferDone: &pb.TransferDone{Filename: filename, TotalBytes: 0},
		},
	}); err != nil {
		return
	}

	pr, pw := io.Pipe()

	// Goroutine: walk directory and write zip data into the pipe
	go func() {
		zw := zip.NewWriter(pw)
		err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			relPath, _ := filepath.Rel(dirPath, path)
			if relPath == "." {
				return nil
			}

			info, ok := zipEntryInfo(resolvedRoot, path, info)
			if !ok {
				return nil
			}

			header, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			header.Name = filepath.ToSlash(relPath)
			if info.IsDir() {
				header.Name += "/"
			} else {
				header.Method = zip.Deflate
			}

			writer, err := zw.CreateHeader(header)
			if err != nil {
				return err
			}

			if !info.IsDir() {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = io.Copy(writer, f)
				return err
			}
			return nil
		})

		zw.Close()
		if err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
	}()

	// Main thread: read from pipe in chunks and send immediately
	buf := make([]byte, chunkSize)
	var offset int64

	for {
		n, readErr := pr.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := sendFn(&pb.NodeMessage{
				RequestId: reqID,
				Payload: &pb.NodeMessage_Chunk{
					Chunk: &pb.DataChunk{Data: chunk, Offset: offset},
				},
			}); err != nil {
				pr.Close()
				return
			}
			offset += int64(n)
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Printf("gRPC: zip streaming error: %v", readErr)
				sendFn(errorMsg(reqID, 500, fmt.Sprintf("zip stream: %v", readErr)))
				return
			}
			break
		}
	}

	// Final TransferDone with TotalBytes > 0 signals completion
	sendFn(&pb.NodeMessage{
		RequestId: reqID,
		Payload: &pb.NodeMessage_TransferDone{
			TransferDone: &pb.TransferDone{TotalBytes: offset, Filename: filename},
		},
	})
}

// streamSelectiveZip zips only selected paths from a base directory using io.Pipe streaming.
// If selectAll is true, zips everything (same as streamDirAsZip).
// This is reusable for backup creation (same SelectiveReadReq message).
func (h *StreamHandler) streamSelectiveZip(reqID, serverUUID string, req *pb.SelectiveReadReq, sendFn func(*pb.NodeMessage) error) {
	basePath, err := h.validatePath(req.BasePath, serverUUID)
	if err != nil {
		sendFn(errorMsg(reqID, 403, err.Error()))
		return
	}

	// If select_all, just zip the whole directory
	if req.SelectAll {
		h.streamDirAsZip(reqID, serverUUID, basePath, sendFn)
		return
	}

	filename := "download.zip"
	if base := filepath.Base(basePath); base != "." && base != "/" {
		filename = base + ".zip"
	}

	// Send metadata first
	if err := sendFn(&pb.NodeMessage{
		RequestId: reqID,
		Payload: &pb.NodeMessage_TransferDone{
			TransferDone: &pb.TransferDone{Filename: filename, TotalBytes: 0},
		},
	}); err != nil {
		return
	}

	resolvedRoot := resolveZipRoot(h.serverDir(serverUUID))
	pr, pw := io.Pipe()

	go func() {
		zw := zip.NewWriter(pw)
		var walkErr error

		for _, sel := range req.Selected {
			// Security: ensure each selected entry stays within basePath
			// (trailing-separator containment, shared with validatePath).
			selPath, err := resolveWithinDir(basePath, sel)
			if err != nil {
				continue
			}

			// Lstat, not Stat: a selected entry that is ITSELF a symlink has to
			// be judged as a link, and Stat would already have followed it past
			// the containment check below.
			linkInfo, err := os.Lstat(selPath)
			if err != nil {
				continue
			}
			stat, ok := zipEntryInfo(resolvedRoot, selPath, linkInfo)
			if !ok {
				continue
			}

			if stat.IsDir() {
				// Walk entire subdirectory
				walkErr = filepath.Walk(selPath, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					relPath, _ := filepath.Rel(basePath, path)
					info, ok := zipEntryInfo(resolvedRoot, path, info)
					if !ok {
						return nil
					}
					header, err := zip.FileInfoHeader(info)
					if err != nil {
						return err
					}
					header.Name = filepath.ToSlash(relPath)
					if info.IsDir() {
						header.Name += "/"
					} else {
						header.Method = zip.Deflate
					}
					writer, err := zw.CreateHeader(header)
					if err != nil {
						return err
					}
					if !info.IsDir() {
						f, err := os.Open(path)
						if err != nil {
							return err
						}
						defer f.Close()
						_, err = io.Copy(writer, f)
						return err
					}
					return nil
				})
				if walkErr != nil {
					break
				}
			} else {
				// Single file
				relPath, _ := filepath.Rel(basePath, selPath)
				header, err := zip.FileInfoHeader(stat)
				if err != nil {
					continue
				}
				header.Name = filepath.ToSlash(relPath)
				header.Method = zip.Deflate
				writer, err := zw.CreateHeader(header)
				if err != nil {
					continue
				}
				f, err := os.Open(selPath)
				if err != nil {
					continue
				}
				io.Copy(writer, f)
				f.Close()
			}
		}

		zw.Close()
		if walkErr != nil {
			pw.CloseWithError(walkErr)
		} else {
			pw.Close()
		}
	}()

	// Stream from pipe
	buf := make([]byte, chunkSize)
	var offset int64

	for {
		n, readErr := pr.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := sendFn(&pb.NodeMessage{
				RequestId: reqID,
				Payload: &pb.NodeMessage_Chunk{
					Chunk: &pb.DataChunk{Data: chunk, Offset: offset},
				},
			}); err != nil {
				pr.Close()
				return
			}
			offset += int64(n)
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Printf("gRPC: selective zip streaming error: %v", readErr)
				sendFn(errorMsg(reqID, 500, fmt.Sprintf("zip stream: %v", readErr)))
				return
			}
			break
		}
	}

	sendFn(&pb.NodeMessage{
		RequestId: reqID,
		Payload: &pb.NodeMessage_TransferDone{
			TransferDone: &pb.TransferDone{TotalBytes: offset, Filename: filename},
		},
	})
}

func (h *StreamHandler) handleWrite(reqID, serverUUID string, req *pb.WriteFileReq) *pb.NodeMessage {
	if isProtectedFile(req.Path) {
		return errorMsg(reqID, 403, "cannot modify protected file")
	}
	filePath, err := h.validatePath(req.Path, serverUUID)
	if err != nil {
		return errorMsg(reqID, 403, err.Error())
	}

	// Ensure parent directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return errorMsg(reqID, 500, fmt.Sprintf("mkdir: %v", err))
	}

	return &pb.NodeMessage{
		RequestId: reqID,
		Payload:   &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "ready for chunks"}},
	}
}

// resolveFinalPath validates and returns the absolute path for a file write.
// Creates parent directories as needed.
func (h *StreamHandler) resolveFinalPath(serverUUID, path string) (string, error) {
	filePath, err := h.validatePath(path, serverUUID)
	if err != nil {
		return "", err
	}
	os.MkdirAll(filepath.Dir(filePath), 0755)
	return filePath, nil
}

// createUploadTemp opens the staging file for one inbound mesh upload.
//
// It is created in the FINAL directory, not a shared temp dir: the transfer
// completes with an os.Rename, and as soon as STORAGE_PATHS points at its own
// mount, a staging dir under the working directory is a different filesystem,
// so that rename fails with EXDEV and every upload 500s. Same approach the beam
// upload path already takes. The leading dot keeps the partial file out of the
// way in the file browser.
func (h *StreamHandler) createUploadTemp(serverUUID, path string) (*os.File, error) {
	finalPath, err := h.resolveFinalPath(serverUUID, path)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(finalPath), ".upload-*.tmp")
	if err != nil {
		return nil, err
	}
	// Chowned as the TEMP file, because the rename that follows carries the
	// ownership with it. Doing it after the rename would be a second window in
	// which the finished file is root's.
	chownForMC(f.Name())
	return f, nil
}

func (h *StreamHandler) handleCreate(reqID, serverUUID string, req *pb.CreateFileReq) *pb.NodeMessage {
	if isProtectedFile(req.Path) {
		return errorMsg(reqID, 403, "cannot modify protected file")
	}
	fullPath, err := h.validatePath(req.Path, serverUUID)
	if err != nil {
		return errorMsg(reqID, 403, err.Error())
	}

	if req.IsDir {
		if err := os.MkdirAll(fullPath, 0755); err != nil {
			return errorMsg(reqID, 500, fmt.Sprintf("mkdir: %v", err))
		}
	} else {
		dir := filepath.Dir(fullPath)
		os.MkdirAll(dir, 0755)
		f, err := os.Create(fullPath)
		if err != nil {
			return errorMsg(reqID, 500, fmt.Sprintf("create: %v", err))
		}
		f.Close()
	}
	// Created by the node as root, into a server that runs as uid 1000. Without
	// this the panel can create a file the server cannot then write.
	chownForMC(fullPath)

	return &pb.NodeMessage{
		RequestId: reqID,
		Payload:   &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "created"}},
	}
}

func (h *StreamHandler) handleDelete(reqID, serverUUID string, req *pb.DeleteFileReq) *pb.NodeMessage {
	if isProtectedFile(req.Path) {
		return errorMsg(reqID, 403, "cannot delete protected file")
	}
	fullPath, err := h.validatePath(req.Path, serverUUID)
	if err != nil {
		return errorMsg(reqID, 403, err.Error())
	}

	if err := os.RemoveAll(fullPath); err != nil {
		return errorMsg(reqID, 500, fmt.Sprintf("delete: %v", err))
	}

	return &pb.NodeMessage{
		RequestId: reqID,
		Payload:   &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "deleted"}},
	}
}

func (h *StreamHandler) handleRename(reqID, serverUUID string, req *pb.RenameFileReq) *pb.NodeMessage {
	if isProtectedFile(req.OldPath) {
		return errorMsg(reqID, 403, "cannot rename protected file")
	}
	oldPath, err := h.validatePath(req.OldPath, serverUUID)
	if err != nil {
		return errorMsg(reqID, 403, err.Error())
	}

	// New name is just the filename, keep same parent directory
	dir := filepath.Dir(oldPath)
	newName := sanitizeFilename(req.NewName)
	if newName == "" {
		return errorMsg(reqID, 400, "invalid filename")
	}
	// The destination gets the same check as the source, not a hand-picked
	// subset of it. This used to compare against ".active_server" alone while
	// isProtectedFile knows four names plus two prefixes, and sanitizeFilename
	// keeps '.', '-' and '_', so .node_config.json, .dylaris.json and
	// .dylaris-backups all survived it intact and could be renamed over.
	if isProtectedFile(newName) {
		return errorMsg(reqID, 403, "cannot use protected filename")
	}
	newPath := filepath.Join(dir, newName)

	if err := os.Rename(oldPath, newPath); err != nil {
		return errorMsg(reqID, 500, fmt.Sprintf("rename: %v", err))
	}

	return &pb.NodeMessage{
		RequestId: reqID,
		Payload:   &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "renamed"}},
	}
}

func (h *StreamHandler) handleCopy(reqID, serverUUID string, req *pb.CopyFileReq) *pb.NodeMessage {
	if isProtectedFile(req.DstPath) {
		return errorMsg(reqID, 403, "cannot overwrite protected file")
	}
	srcPath, err := h.validatePath(req.SrcPath, serverUUID)
	if err != nil {
		return errorMsg(reqID, 403, err.Error())
	}
	dstPath, err := h.validatePath(req.DstPath, serverUUID)
	if err != nil {
		return errorMsg(reqID, 403, err.Error())
	}
	if err := validateCopyPaths(req.SrcPath, req.DstPath, srcPath, dstPath); err != nil {
		return errorMsg(reqID, 400, err.Error())
	}

	stat, err := os.Stat(srcPath)
	if err != nil {
		return errorMsg(reqID, 404, "source not found")
	}

	if stat.IsDir() {
		if err := copyDir(srcPath, dstPath); err != nil {
			return errorMsg(reqID, 500, fmt.Sprintf("copy dir: %v", err))
		}
	} else {
		if err := copyFileForTenant(srcPath, dstPath); err != nil {
			return errorMsg(reqID, 500, fmt.Sprintf("copy file: %v", err))
		}
	}

	return &pb.NodeMessage{
		RequestId: reqID,
		Payload:   &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "copied"}},
	}
}

// handleInspectOrphan returns metadata, active sub-server, and sub-server scan
// for a server UUID. Pure read — performs no mutation.
func (h *StreamHandler) handleInspectOrphan(reqID, serverUUID string) *pb.NodeMessage {
	if serverUUID == "" {
		return errorMsg(reqID, 400, "server_uuid required")
	}

	var serverDir string
	if h.storageMgr != nil {
		serverDir = h.storageMgr.GetServerDir(serverUUID)
	} else {
		serverDir = filepath.Join(h.baseDir, "dylaris_data", "servers", serverUUID)
	}

	resp := &pb.InspectOrphanResp{}

	// Read .dylaris.json — missing or malformed is not an error, just has_metadata=false.
	if m, err := readServerMetadata(serverDir); err == nil {
		data, jsonErr := json.Marshal(m)
		if jsonErr == nil {
			resp.HasMetadata = true
			resp.MetadataJson = string(data)
		} else {
			log.Printf("handleInspectOrphan: marshal metadata for %s: %v", serverUUID, jsonErr)
		}
	}

	// Read .active_server — absent is fine, leave ActiveSubServer as "".
	activeBytes, err := os.ReadFile(filepath.Join(serverDir, ".active_server"))
	if err == nil {
		resp.ActiveSubServer = strings.TrimSpace(string(activeBytes))
	}

	// Scan sub-server directories.
	// SubServerInfo intentionally carries only name + type; the full
	// per-sub-server fields (minecraft_version, build, extra_jvm_flags) are
	// available to Core via the metadata_json field of the response.
	for _, sub := range scanSubServers(serverDir) {
		resp.SubServers = append(resp.SubServers, &pb.SubServerInfo{
			Name: sub.Name,
			Type: sub.Type,
		})
	}

	return &pb.NodeMessage{
		RequestId: reqID,
		Payload:   &pb.NodeMessage_InspectOrphanResp{InspectOrphanResp: resp},
	}
}

// --- Helpers ---

// isProtectedFile checks if a path points to a protected system file.
//
// Used by the gRPC file handlers and the SFTP virtual-FS adapter to reject
// writes/deletes/renames against names the platform manages internally.
// SFTP additionally uses this set as a *listing* filter (entries excluded
// here never show up in Readdir / Stat output), which is why
// ".dylaris-backups" is included — node-local backups must stay completely
// invisible from the SFTP file browser, including the parent directory.
//
// The check also fires when ANY component of the path is .dylaris-backups,
// so a user that somehow guesses ".dylaris-backups/foo.tar.gz" can't write
// or rename it through the regular file API. The dedicated backup RPCs
// (handleBackupList/Open/Delete in grpc_backup.go) reach those files
// without going through this guard.
func isProtectedFile(path string) bool {
	clean := filepath.Clean(path)
	// The server root itself. An empty path is legal on the read side, where
	// it means "list the server directory", and every destructive handler
	// resolves it to that same directory: delete would RemoveAll the server
	// and its backups, rename would move the whole directory out from under
	// its UUID, copy would walk the tree onto itself. None of those is a file
	// operation a caller may perform, so the root is protected like the
	// dotfiles inside it.
	if clean == "." || clean == string(filepath.Separator) || clean == "/" {
		return true
	}
	name := filepath.Base(clean)
	if name == ".active_server" || name == ".node_config.json" || name == ".dylaris-backups" || name == ".dylaris.json" {
		return true
	}
	// .pending-delete-* are short-lived rename targets used by the
	// sub-server delete pipeline -- the dir is moved here so the
	// browser stops seeing the original name immediately, with
	// async RemoveAll cleaning up the tombstone in the background.
	// Either way the user has no business seeing it.
	if strings.HasPrefix(name, ".pending-delete-") {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".dylaris-backups" || strings.HasPrefix(part, ".pending-delete-") {
			return true
		}
		// A Technic install's staging and download directories. The installer
		// moves files out of them as root; a tenant who could reach in during
		// the install could swap an entry for a symlink between two steps.
		if strings.HasPrefix(part, ".technic-stage-") || strings.HasPrefix(part, ".technic-dl-") {
			return true
		}
	}
	return false
}

// validateCopyPaths rejects the copy requests that destroy data instead of
// duplicating it. Both the control-plane RPC and the beam RPC go through it.
//
// An empty path resolves to the server root, which is correct for listing and
// catastrophic for copying: src and dst both become the root, filepath.Walk
// visits every file, and copyFile opens each one for writing (O_TRUNC) as its
// own source. One copy request with two empty paths zeroed 185 files here -
// the whole world, every config, the three dotfiles isProtectedFile exists to
// defend, and both backup archives, which live under the same root. The
// restore then died on "gzip open: EOF". That guard never fired: it only sees
// the destination string, and filepath.Base("") is ".".
//
// rawSrc/rawDst are the request's paths, used only for the empty check; src and
// dst are the resolved absolute paths.
func validateCopyPaths(rawSrc, rawDst, src, dst string) error {
	if strings.TrimSpace(rawSrc) == "" || strings.TrimSpace(rawDst) == "" {
		return fmt.Errorf("copy needs both a source and a destination path")
	}
	src = filepath.Clean(src)
	dst = filepath.Clean(dst)
	if src == dst {
		return fmt.Errorf("cannot copy a path onto itself")
	}
	// dst inside src would copy the tree into itself, walking what it writes.
	if rel, err := filepath.Rel(src, dst); err == nil &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("cannot copy a directory into itself")
	}
	return nil
}

func errorMsg(reqID string, code int32, message string) *pb.NodeMessage {
	return &pb.NodeMessage{
		RequestId: reqID,
		Payload: &pb.NodeMessage_Error{
			Error: &pb.OpError{Code: code, Message: message},
		},
	}
}

func sanitizeFilename(name string) string {
	var result []byte
	for _, c := range []byte(name) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' || c == '+' {
			result = append(result, c)
		}
	}
	return string(result)
}

// copyFile and copyDir are defined in installer.go — reused here.
