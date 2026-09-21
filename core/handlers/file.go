package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"dylaris-core/authz"
	"dylaris-core/services"
	"dylaris-pkg/beam/quota"
	"dylaris-pkg/validate"
	pb "dylaris-proto/node"

	"github.com/google/uuid"
)

// FileInfo is a struct to hold file and directory information
type FileInfo struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// FileHandler is the handler for file-related API calls
type FileHandler struct {
	RootPath string
	state    *AppState
}

// Regex for sanitizing filenames
var validFilenameRegex = regexp.MustCompile(`[^a-zA-Z0-9._\-+]`)

func sanitizeFilename(name string) string {
	return validFilenameRegex.ReplaceAllString(name, "_")
}

func NewFileHandler(state *AppState) *FileHandler {
	return &FileHandler{RootPath: "servers", state: state}
}

// getTransferLimit returns the upload or download cap for the current user, on
// the platform limit convention: nil is no limit, 0 refuses the transfer, n is
// the ceiling in bytes.
//
// It used to return a bare int64 and read the setting with `n > 0`, so a stored
// 0 - the one value meaning "none" - fell through to the built-in default. An
// operator who set a user upload limit of zero granted 500 MB, and "no limit"
// could not be expressed at all.
//
// The defaults live in ONE place now (defaultFileManagerSettings) instead of
// being repeated here: the two copies were already free to drift, and the second
// one is invisible from the settings screen that appears to own them.
func (h *FileHandler) getTransferLimit(r *http.Request, limitType string) *int64 {
	isAdmin := IsAdmin(r)
	var key string
	var def *int64
	if limitType == "upload" {
		if isAdmin {
			key, def = "fm.admin_upload_limit", defaultFileManagerSettings.AdminUploadLimit
		} else {
			key, def = "fm.user_upload_limit", defaultFileManagerSettings.UserUploadLimit
		}
	} else {
		if isAdmin {
			key, def = "fm.admin_download_limit", defaultFileManagerSettings.AdminDownloadLimit
		} else {
			key, def = "fm.user_download_limit", defaultFileManagerSettings.UserDownloadLimit
		}
	}
	val, err := h.state.Store.GetSetting(key)
	if err != nil {
		return def
	}
	return services.ParseLimitSetting(val, def)
}

// downloadBudget enforces the per-user download ceiling across a streamed
// response whose total size is not known up front (the node zips a directory
// as it walks it, so there is no Content-Length to pre-check). The only place
// the limit can be applied is per chunk, as the bytes go past.
//
// getTransferLimit has had a "download" branch, and settings.go has read and
// written fm.user_download_limit / fm.admin_download_limit, since the file
// manager existed - but nothing ever called the download branch. The panel
// offered the operator a download ceiling that did nothing, while the upload
// ceiling right next to it (same function, same settings page) was enforced on
// both write paths.
// downloadBudget tracks what is left of a download ceiling. unlimited is the
// nil cap: no ceiling to spend, so take always succeeds. A budget of 0 is a real
// ceiling of none and refuses the first byte, which is the difference the
// pointer exists to carry.
type downloadBudget struct {
	left      int64
	unlimited bool
}

// newDownloadBudget builds one from a cap on the platform limit convention.
func newDownloadBudget(cap *int64) downloadBudget {
	if cap == nil {
		return downloadBudget{unlimited: true}
	}
	return downloadBudget{left: *cap}
}

// take reserves n bytes, reporting false once the budget is spent.
func (b *downloadBudget) take(n int) bool {
	if b.unlimited {
		return true
	}
	if b.left < int64(n) {
		return false
	}
	b.left -= int64(n)
	return true
}

// refuse ends a download that hit the ceiling. Before any byte is written
// there is still a status code to send. After that there is not: the abort is
// what makes the browser report a failed download instead of silently saving a
// truncated file, and net/http treats ErrAbortHandler as a deliberate abort
// (no stack trace, no 500 attempt on a response already in flight).
func (b *downloadBudget) refuse(w http.ResponseWriter, bodyStarted bool) {
	if !bodyStarted {
		// The attachment headers were staged from the node's metadata frame
		// before the first chunk arrived; leaving them on would make the
		// browser offer to SAVE this error message as the requested file.
		w.Header().Del("Content-Disposition")
		http.Error(w, "This download exceeds your download limit", http.StatusRequestEntityTooLarge)
		return
	}
	panic(http.ErrAbortHandler)
}

// getServerUUID extracts and validates the server_uuid query param for a WRITE
// or download operation against requiredCap (files.write or files.delete).
// Non-admin users must own the server or hold requiredCap via a grant. Demo
// viewers are denied here (read-only).
func (h *FileHandler) getServerUUID(r *http.Request, requiredCap string) (string, error) {
	uuid, _, err := h.resolveServerUUID(r, false, requiredCap)
	return uuid, err
}

// getServerUUIDRead is getServerUUID for read-only operations (list + view file
// content), always resolved against files.read. It additionally allows any
// authenticated user to READ a demo server, so a logged-out-of-everything
// account can still browse the showcase. Write and download endpoints keep
// using the strict getServerUUID. viaDemoBypass reports whether access was
// granted THROUGH the demo bypass rather than real ownership or a grant, so
// callers can redact sensitive file content on that path.
func (h *FileHandler) getServerUUIDRead(r *http.Request) (uuid string, viaDemoBypass bool, err error) {
	return h.resolveServerUUID(r, true, "files.read")
}

func (h *FileHandler) resolveServerUUID(r *http.Request, allowDemoRead bool, requiredCap string) (uuid string, viaDemoBypass bool, err error) {
	uuid = r.URL.Query().Get("server_uuid")
	if uuid == "" {
		uuid = r.FormValue("server_uuid")
	}
	if uuid == "" {
		return "", false, nil
	}
	if h.state == nil || h.state.Store == nil {
		return uuid, false, nil
	}
	username, _ := r.Context().Value("username").(string)
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	userID, _ := r.Context().Value("userID").(string)
	// No admin short-circuit here: the resolver already opens everything to an
	// admin except a server on somebody else's machine, and this route answering
	// before it asked was how an admin read and wrote a customer's files anyway.
	srv, err := h.state.Store.GetServerByUUID(uuid)
	if err != nil {
		return "", false, fmt.Errorf("server not found")
	}
	// Route through the same capability resolver every other server-scoped
	// route uses: owner short-circuit, or a direct/proxy/account grant holding
	// requiredCap. files.read is excluded from the resolver's demo-read grant
	// (see authz.demoReadDeny), so a demo stranger never passes here on reads -
	// they fall into the demo-bypass branch below, which is redaction-aware.
	res, rerr := h.state.Authz.Resolve(authz.Identity{UserID: userID, Username: username, IsAdmin: isAdmin}, srv.ID)
	if rerr != nil || !res.HasCap(requiredCap) {
		if allowDemoRead && isDemoServer(h.state, uuid) {
			return uuid, true, nil // read-only demo access; redaction downstream via viaDemoBypass
		}
		return "", false, fmt.Errorf("access denied")
	}
	return uuid, false, nil
}

// demoHiddenNotice stands in for the content of a file the demo bypass may not
// show. It is deliberately a normal string, not an error: the file browser is
// what the demo exists to show off, so opening a file has to keep working.
const demoHiddenNotice = "This file is not shown on the demo server.\n\n" +
	"Only server.properties and eula.txt are. Every other file on a Minecraft server may carry\n" +
	"credentials - database passwords in plugin configuration, a proxy forwarding secret, player\n" +
	"identities - and the demo is readable by anyone with an account.\n"

// demoFileContent decides what a file's content looks like when it was read via
// the demo-server bypass, which lets ANY authenticated user read a flagged
// showcase server (see resolveServerUUID). Two files are shown; everything else
// gets demoHiddenNotice.
//
// This used to be the other way round - everything was shown except
// server.properties' rcon.password and ops.json - and the shape is why it
// changed rather than the two names being wrong. Naming what must be hidden
// requires having thought of it first, and a Minecraft server keeps secrets in
// files nobody enumerated: plugins/LuckPerms/config.yml holds MySQL
// credentials, a Velocity forwarding.secret lets its bearer join as any player,
// half the plugins on a real server store an API token in their own config.yml.
// Every one of those came back verbatim, to any account, with nothing failing.
//
// The listing is untouched: names, sizes and the directory tree are still the
// whole file browser, which is what the demo is showing.
func demoFileContent(path, content string) string {
	base := filepath.Base(strings.ReplaceAll(path, "\\", "/"))
	switch base {
	case "server.properties":
		// Shown because it is the file a visitor expects to look at, minus the
		// one line in it that is a credential.
		lines := strings.Split(content, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "rcon.password=") {
				lines[i] = "rcon.password=REDACTED"
			}
		}
		return strings.Join(lines, "\n")
	case "eula.txt":
		return content
	default:
		return demoHiddenNotice
	}
}

// getNodeIDForServer looks up which Node owns this server and returns its NodeID.
func (h *FileHandler) getNodeIDForServer(serverUUID string) (int, error) {
	srv, err := h.state.Store.GetServerByUUID(serverUUID)
	if err != nil {
		return 0, fmt.Errorf("server not found: %w", err)
	}
	return srv.NodeID, nil
}

// sendGRPCMsg sends a pre-built NodeMessage via gRPC and waits for a response.
func (h *FileHandler) sendGRPCMsg(nodeID int, msg *pb.NodeMessage, timeout time.Duration) (*pb.NodeMessage, error) {
	if h.state.GRPCRegistry == nil {
		return nil, fmt.Errorf("gRPC registry not initialized")
	}
	return h.state.GRPCRegistry.SendRequest(nodeID, msg, timeout)
}

// GetFilesHandler handles requests to list files in a directory
func (h *FileHandler) GetFilesHandler(w http.ResponseWriter, r *http.Request) {
	pathParam := r.URL.Query().Get("path")
	if pathParam == "" {
		pathParam = "/"
	}
	serverUUID, _, err := h.getServerUUIDRead(r)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusForbidden)
		return
	}
	if serverUUID == "" {
		sendJSONError(w, "server_uuid required", http.StatusBadRequest)
		return
	}

	nodeID, err := h.getNodeIDForServer(serverUUID)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusNotFound)
		return
	}

	resp, err := h.sendGRPCMsg(nodeID, &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_ListReq{ListReq: &pb.ListFilesReq{Path: pathParam}},
	}, 30*time.Second)
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Node communication error: %v", err), http.StatusBadGateway)
		return
	}

	if errResp := resp.GetError(); errResp != nil {
		if errResp.Code == 403 {
			sendJSONError(w, errResp.Message, http.StatusForbidden)
		} else {
			sendJSONError(w, errResp.Message, int(errResp.Code))
		}
		return
	}

	listResp := resp.GetListResp()
	if listResp == nil {
		sendJSONError(w, "unexpected response from node", http.StatusInternalServerError)
		return
	}

	fileList := make([]FileInfo, 0, len(listResp.Files))
	for _, f := range listResp.Files {
		fileList = append(fileList, FileInfo{
			Name:  f.Name,
			IsDir: f.IsDir,
			Size:  f.Size,
		})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"files":   fileList,
	})
}

// GetFileContentHandler handles requests to read the content of a file
func (h *FileHandler) GetFileContentHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	serverUUID, viaDemoBypass, err := h.getServerUUIDRead(r)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusForbidden)
		return
	}
	if serverUUID == "" {
		sendJSONError(w, "server_uuid required", http.StatusBadRequest)
		return
	}

	nodeID, err := h.getNodeIDForServer(serverUUID)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusNotFound)
		return
	}

	// Use streaming request to receive file chunks
	reqID := uuid.NewString()
	msg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload: &pb.NodeMessage_ReadReq{
			ReadReq: &pb.ReadFileReq{Path: path, ZipIfDir: false},
		},
	}

	ch, err := h.state.GRPCRegistry.SendRequestStreaming(nodeID, msg)
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Node communication error: %v", err), http.StatusBadGateway)
		return
	}
	defer h.state.GRPCRegistry.CleanupRequest(nodeID, reqID)

	// Collect all chunks into a buffer
	var buf bytes.Buffer
	for resp := range ch {
		if errResp := resp.GetError(); errResp != nil {
			sendJSONError(w, errResp.Message, int(errResp.Code))
			return
		}
		if chunk := resp.GetChunk(); chunk != nil {
			buf.Write(chunk.Data)
		}
		// TransferDone signals end — channel will be closed by registry
	}

	content := buf.String()
	if viaDemoBypass {
		// Any authenticated user can reach this via the demo bypass, not just
		// the owner, so only the two files the demo exists to show come back.
		content = demoFileContent(path, content)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"content": content,
	})
}

// SaveFileHandler handles requests to save file content
func (h *FileHandler) SaveFileHandler(w http.ResponseWriter, r *http.Request) {
	// Bound the request body: the whole file content arrives inline as JSON, so
	// without this an authenticated user could POST an arbitrarily large body and
	// have it decoded into RAM. Reuse the per-user upload ceiling (the same one
	// UploadFileHandler applies via MaxBytesReader).
	if !capBodyLimit(w, r, h.getTransferLimit(r, "upload")) {
		return
	}

	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			sendJSONError(w, "File is too large or exceeds your upload limit", http.StatusRequestEntityTooLarge)
			return
		}
		sendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if !validate.IsSafeRelPath(req.Path) {
		sendJSONError(w, "Invalid path", http.StatusBadRequest)
		return
	}

	serverUUID, err := h.getServerUUID(r, "files.write")
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusForbidden)
		return
	}
	if serverUUID == "" {
		sendJSONError(w, "server_uuid required", http.StatusBadRequest)
		return
	}

	nodeID, err := h.getNodeIDForServer(serverUUID)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusNotFound)
		return
	}

	conn, ok := h.state.GRPCRegistry.GetConnection(nodeID)
	if !ok {
		sendJSONError(w, fmt.Sprintf("Node %d not connected", nodeID), http.StatusBadGateway)
		return
	}

	reqID := uuid.NewString()
	data := []byte(req.Content)

	// Step 1: Send WriteFileReq
	writeReqMsg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload: &pb.NodeMessage_WriteReq{
			WriteReq: &pb.WriteFileReq{Path: req.Path, TotalSize: int64(len(data))},
		},
	}

	resp, err := h.state.GRPCRegistry.SendRequest(nodeID, writeReqMsg, 30*time.Second)
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Node communication error: %v", err), http.StatusBadGateway)
		return
	}
	if errResp := resp.GetError(); errResp != nil {
		sendJSONError(w, errResp.Message, int(errResp.Code))
		return
	}

	// Step 2: Send data chunks
	const chunkSize = 64 * 1024
	for offset := 0; offset < len(data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunkMsg := &pb.NodeMessage{
			RequestId:  reqID,
			ServerUuid: serverUUID,
			Payload: &pb.NodeMessage_Chunk{
				Chunk: &pb.DataChunk{Data: data[offset:end], Offset: int64(offset)},
			},
		}
		if err := conn.Send(chunkMsg); err != nil {
			sendJSONError(w, fmt.Sprintf("Failed to send chunk: %v", err), http.StatusBadGateway)
			return
		}
	}

	// Step 3: Send TransferDone
	doneMsg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload: &pb.NodeMessage_TransferDone{
			TransferDone: &pb.TransferDone{TotalBytes: int64(len(data))},
		},
	}
	if err := conn.Send(doneMsg); err != nil {
		sendJSONError(w, fmt.Sprintf("Failed to send transfer done: %v", err), http.StatusBadGateway)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "File saved successfully",
	})
}

// CreateFileHandler handles requests to create a new file or directory
func (h *FileHandler) CreateFileHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if !validate.IsSafeRelPath(req.Path) {
		sendJSONError(w, "Invalid path", http.StatusBadRequest)
		return
	}
	dir, file := filepath.Split(req.Path)
	cleanFile := sanitizeFilename(file)
	req.Path = filepath.Join(dir, cleanFile)

	serverUUID, err := h.getServerUUID(r, "files.write")
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusForbidden)
		return
	}
	if serverUUID == "" {
		sendJSONError(w, "server_uuid required", http.StatusBadRequest)
		return
	}

	nodeID, err := h.getNodeIDForServer(serverUUID)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusNotFound)
		return
	}

	resp, err := h.sendGRPCMsg(nodeID, &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_CreateReq{CreateReq: &pb.CreateFileReq{Path: req.Path, IsDir: req.IsDir}},
	}, 30*time.Second)
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Node communication error: %v", err), http.StatusBadGateway)
		return
	}
	if errResp := resp.GetError(); errResp != nil {
		sendJSONError(w, errResp.Message, int(errResp.Code))
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// RenameFileHandler handles renaming files and directories
func (h *FileHandler) RenameFileHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPath string `json:"oldPath"`
		NewPath string `json:"newPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// oldPath went to the node unchecked. The node validates it, but an empty
	// oldPath resolves to the server directory there, and a rename of that
	// moves the whole server out from under its UUID.
	if !validate.IsSafeRelPath(req.OldPath) || strings.TrimSpace(req.OldPath) == "" {
		sendJSONError(w, "Invalid path", http.StatusBadRequest)
		return
	}
	_, newFile := filepath.Split(req.NewPath)
	cleanFile := sanitizeFilename(newFile)

	serverUUID, err := h.getServerUUID(r, "files.write")
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusForbidden)
		return
	}
	if serverUUID == "" {
		sendJSONError(w, "server_uuid required", http.StatusBadRequest)
		return
	}

	nodeID, err := h.getNodeIDForServer(serverUUID)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusNotFound)
		return
	}

	resp, err := h.sendGRPCMsg(nodeID, &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_RenameReq{RenameReq: &pb.RenameFileReq{OldPath: req.OldPath, NewName: cleanFile}},
	}, 30*time.Second)
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Node communication error: %v", err), http.StatusBadGateway)
		return
	}
	if errResp := resp.GetError(); errResp != nil {
		sendJSONError(w, errResp.Message, int(errResp.Code))
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// CopyFileHandler handles copying files and directories
func (h *FileHandler) CopyFileHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPath string `json:"oldPath"`
		NewPath string `json:"newPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	serverUUID, err := h.getServerUUID(r, "files.write")
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusForbidden)
		return
	}
	if serverUUID == "" {
		sendJSONError(w, "server_uuid required", http.StatusBadRequest)
		return
	}

	nodeID, err := h.getNodeIDForServer(serverUUID)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusNotFound)
		return
	}

	// Copy was the one file handler with no path check at all, while save,
	// create, rename and delete all have one. An empty path is legal
	// everywhere else (it means the server root, which is what a listing
	// wants) and is destructive here: the node resolves both ends to the root
	// and copies the tree onto itself, truncating every file it walks.
	if !validate.IsSafeRelPath(req.OldPath) || !validate.IsSafeRelPath(req.NewPath) {
		sendJSONError(w, "Invalid path", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.OldPath) == "" || strings.TrimSpace(req.NewPath) == "" {
		sendJSONError(w, "Source and destination path are required", http.StatusBadRequest)
		return
	}

	resp, err := h.sendGRPCMsg(nodeID, &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_CopyReq{CopyReq: &pb.CopyFileReq{SrcPath: req.OldPath, DstPath: req.NewPath}},
	}, 60*time.Second)
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Node communication error: %v", err), http.StatusBadGateway)
		return
	}
	if errResp := resp.GetError(); errResp != nil {
		sendJSONError(w, errResp.Message, int(errResp.Code))
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// DeleteFileHandler handles requests to delete a file
func (h *FileHandler) DeleteFileHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	serverUUID, err := h.getServerUUID(r, "files.delete")
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusForbidden)
		return
	}
	if serverUUID == "" {
		sendJSONError(w, "server_uuid required", http.StatusBadRequest)
		return
	}

	nodeID, err := h.getNodeIDForServer(serverUUID)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusNotFound)
		return
	}

	// IsSafeRelPath accepts "" on purpose: on the read side it means "the
	// server directory". Here it means RemoveAll on that directory, backups
	// and all. The node refuses it too; refusing early keeps the request from
	// ever reaching a node running an older build.
	if !validate.IsSafeRelPath(req.Path) || strings.TrimSpace(req.Path) == "" {
		sendJSONError(w, "Invalid path", http.StatusBadRequest)
		return
	}
	resp, err := h.sendGRPCMsg(nodeID, &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_DeleteReq{DeleteReq: &pb.DeleteFileReq{Path: req.Path}},
	}, 30*time.Second)
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Node communication error: %v", err), http.StatusBadGateway)
		return
	}
	if errResp := resp.GetError(); errResp != nil {
		sendJSONError(w, errResp.Message, int(errResp.Code))
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// DownloadFileHandler handles file and folder downloads via gRPC streaming
func (h *FileHandler) DownloadFileHandler(w http.ResponseWriter, r *http.Request) {
	pathParam := r.URL.Query().Get("path")
	// The node validates this too. Refusing early keeps the request from ever
	// reaching a node running an older build - the same reason DeleteFileHandler
	// gives. "" is legal and means the server root (download the whole server).
	if !validate.IsSafeRelPath(pathParam) {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	serverUUID, err := h.getServerUUID(r, "files.read")
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if serverUUID == "" {
		http.Error(w, "server_uuid required", http.StatusBadRequest)
		return
	}

	nodeID, err := h.getNodeIDForServer(serverUUID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Request file/dir read with zip_if_dir=true for directories
	reqID := uuid.NewString()
	msg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload: &pb.NodeMessage_ReadReq{
			ReadReq: &pb.ReadFileReq{Path: pathParam, ZipIfDir: true},
		},
	}

	ch, err := h.state.GRPCRegistry.SendRequestStreaming(nodeID, msg)
	if err != nil {
		http.Error(w, fmt.Sprintf("Node communication error: %v", err), http.StatusBadGateway)
		return
	}
	defer h.state.GRPCRegistry.CleanupRequest(nodeID, reqID)

	// Stream chunks to HTTP response.
	// First message is metadata TransferDone (TotalBytes=0) with filename — set headers.
	// Then stream data chunks directly to browser (zero-copy).
	// Final TransferDone (TotalBytes>0) signals completion — channel closes.
	headerWritten := false
	// Distinct from headerWritten: the metadata TransferDone stages headers
	// before a single body byte exists, and only an already-STARTED body takes
	// the status code away from us. See downloadBudget.refuse.
	bodyStarted := false
	flusher, canFlush := w.(http.Flusher)
	budget := newDownloadBudget(h.getTransferLimit(r, "download"))

	for resp := range ch {
		if errResp := resp.GetError(); errResp != nil {
			if !headerWritten {
				http.Error(w, errResp.Message, int(errResp.Code))
			}
			return
		}

		// Metadata TransferDone (TotalBytes=0): set headers before any data
		if done := resp.GetTransferDone(); done != nil && done.TotalBytes == 0 {
			w.Header().Set("Content-Type", "application/octet-stream")
			if done.Filename != "" {
				setAttachmentDisposition(w, done.Filename)
			}
			headerWritten = true
			continue
		}

		if chunk := resp.GetChunk(); chunk != nil {
			if !budget.take(len(chunk.Data)) {
				budget.refuse(w, bodyStarted)
				return
			}
			if !headerWritten {
				w.Header().Set("Content-Type", "application/octet-stream")
				headerWritten = true
			}
			w.Write(chunk.Data)
			bodyStarted = true
			if canFlush {
				flusher.Flush()
			}
		}

		// Final TransferDone (TotalBytes>0): transfer complete, channel will close
	}
}

// SelectiveDownloadHandler handles selective folder downloads.
// GET /api/files/download/selective?server_uuid=...&base_path=...&selected=a&selected=b&select_all=false
func (h *FileHandler) SelectiveDownloadHandler(w http.ResponseWriter, r *http.Request) {
	basePath := r.URL.Query().Get("base_path")
	selectedPaths := r.URL.Query()["selected"]
	selectAll := r.URL.Query().Get("select_all") == "true"

	// Same early refusal as DownloadFileHandler, applied to every path the
	// caller supplies - the node joins each selected entry onto base_path.
	if !validate.IsSafeRelPath(basePath) {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	for _, p := range selectedPaths {
		if !validate.IsSafeRelPath(p) {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}
	}

	serverUUID, err := h.getServerUUID(r, "files.read")
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if serverUUID == "" {
		http.Error(w, "server_uuid required", http.StatusBadRequest)
		return
	}

	if !selectAll && len(selectedPaths) == 0 {
		http.Error(w, "no paths selected", http.StatusBadRequest)
		return
	}

	nodeID, err := h.getNodeIDForServer(serverUUID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	reqID := uuid.NewString()
	msg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload: &pb.NodeMessage_SelectiveReadReq{
			SelectiveReadReq: &pb.SelectiveReadReq{
				BasePath:  basePath,
				Selected:  selectedPaths,
				SelectAll: selectAll,
			},
		},
	}

	ch, err := h.state.GRPCRegistry.SendRequestStreaming(nodeID, msg)
	if err != nil {
		http.Error(w, fmt.Sprintf("Node communication error: %v", err), http.StatusBadGateway)
		return
	}
	defer h.state.GRPCRegistry.CleanupRequest(nodeID, reqID)

	headerWritten := false
	bodyStarted := false
	flusher, canFlush := w.(http.Flusher)
	budget := newDownloadBudget(h.getTransferLimit(r, "download"))

	for resp := range ch {
		if errResp := resp.GetError(); errResp != nil {
			if !headerWritten {
				http.Error(w, errResp.Message, int(errResp.Code))
			}
			return
		}

		if done := resp.GetTransferDone(); done != nil && done.TotalBytes == 0 {
			w.Header().Set("Content-Type", "application/octet-stream")
			if done.Filename != "" {
				setAttachmentDisposition(w, done.Filename)
			}
			headerWritten = true
			continue
		}

		if chunk := resp.GetChunk(); chunk != nil {
			if !budget.take(len(chunk.Data)) {
				budget.refuse(w, bodyStarted)
				return
			}
			if !headerWritten {
				w.Header().Set("Content-Type", "application/octet-stream")
				headerWritten = true
			}
			w.Write(chunk.Data)
			bodyStarted = true
			if canFlush {
				flusher.Flush()
			}
		}
	}
}

// UploadFileHandler handles uploads — receives files via HTTP multipart,
// then streams them to the Node via gRPC chunks.
func (h *FileHandler) UploadFileHandler(w http.ResponseWriter, r *http.Request) {
	uploadLimit := h.getTransferLimit(r, "upload")
	if !capBodyLimit(w, r, uploadLimit) {
		return
	}
	// 32MiB in memory; anything larger spills to a temp file on disk. Passing
	// uploadLimit here would buffer the whole (up to multi-GB) upload in RAM.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		sendJSONError(w, "File is too large or exceeds your upload limit", http.StatusBadRequest)
		return
	}

	// An upload that carries nothing is a failed upload, not a successful one.
	// It used to fall all the way through the loop below and answer 200 with
	// "0 files uploaded successfully" - measured on production by sending the
	// part as "file" rather than "files", which is the obvious mistake to make
	// against this endpoint. The only signal a client had was the 0 in a
	// sentence whose other word was "successfully".
	//
	// It is answered here, before the server and node are looked up: there is
	// nothing to ask a node about.
	if len(r.MultipartForm.File["files"]) == 0 {
		sendJSONError(w, "No files in the request: the file part must be named \"files\"", http.StatusBadRequest)
		return
	}

	path := r.FormValue("path")
	if !validate.IsSafeRelPath(path) {
		sendJSONError(w, "Invalid path", http.StatusBadRequest)
		return
	}
	serverUUID, err := h.getServerUUID(r, "files.write")
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusForbidden)
		return
	}
	if serverUUID == "" {
		sendJSONError(w, "server_uuid required", http.StatusBadRequest)
		return
	}

	// Disk quota pre-check: reject upload if it would exceed the limit
	if h.state.Redis != nil {
		diskKey := fmt.Sprintf("dylaris:server:%s:stats:disk", serverUUID)
		if diskData, err := h.state.Redis.Get(context.Background(), diskKey).Result(); err == nil {
			var diskInfo struct {
				Total int64 `json:"total"`
				Limit int64 `json:"limit"`
			}
			if json.Unmarshal([]byte(diskData), &diskInfo) == nil && diskInfo.Limit > 0 {
				// Sum all file sizes from the upload
				var totalUploadSize int64
				files := r.MultipartForm.File["files"]
				for _, fh := range files {
					totalUploadSize += fh.Size
				}
				freeBytes := diskInfo.Limit - diskInfo.Total
				if freeBytes < 0 {
					freeBytes = 0
				}
				if diskInfo.Total+totalUploadSize > diskInfo.Limit {
					sendJSONError(w, fmt.Sprintf(
						"Speicherlimit erreicht — %s frei, Upload ist %s",
						formatBytesHuman(freeBytes),
						formatBytesHuman(totalUploadSize),
					), http.StatusRequestEntityTooLarge)
					return
				}
			}
		}
	}

	// Beam upload limits (admin-configured), enforced here too so a browser
	// upload cannot evade the size cap + per-user daily quota the node enforces
	// on the beam tunnel path. The username is the same context value the beam
	// ticket carries, so browser and beam uploads share one per-user/day bucket
	// (shared dylaris-pkg/beam/quota). Fail-open, like the disk check above.
	username, _ := r.Context().Value("username").(string)
	sizeCap := quota.MaxUploadCap(r.Context(), h.state.Redis) // read once, not once per file
	var quotaUploadSize int64
	for _, fh := range r.MultipartForm.File["files"] {
		quotaUploadSize += fh.Size
		if quota.ExceedsSizeCap(fh.Size, sizeCap) {
			sendJSONError(w, fmt.Sprintf("Upload of %s exceeds the %s per-file limit",
				formatBytesHuman(fh.Size), formatBytesHuman(*sizeCap)), http.StatusRequestEntityTooLarge)
			return
		}
	}
	if ok, used, limit := quota.CheckDailyQuota(r.Context(), h.state.Redis, username, quotaUploadSize); !ok {
		sendJSONError(w, fmt.Sprintf("Daily upload quota reached — %s of %s used today, upload is %s",
			formatBytesHuman(used), formatBytesHuman(*limit), formatBytesHuman(quotaUploadSize)),
			http.StatusRequestEntityTooLarge)
		return
	}

	nodeID, err := h.getNodeIDForServer(serverUUID)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusNotFound)
		return
	}

	conn, ok := h.state.GRPCRegistry.GetConnection(nodeID)
	if !ok {
		sendJSONError(w, fmt.Sprintf("Node %d not connected", nodeID), http.StatusBadGateway)
		return
	}

	files := r.MultipartForm.File["files"]
	var uploadedBytes int64 // actual bytes streamed, for the daily quota counter
	for _, fileHeader := range files {
		sanitizedName := sanitizeFilename(fileHeader.Filename)
		if sanitizedName == "" || sanitizedName == ".active_server" {
			continue
		}

		file, err := fileHeader.Open()
		if err != nil {
			sendJSONError(w, "Could not open uploaded file", http.StatusInternalServerError)
			return
		}

		filePath := filepath.Join(path, sanitizedName)
		reqID := uuid.NewString()

		// Step 1: WriteFileReq with total size from header
		writeReqMsg := &pb.NodeMessage{
			RequestId:  reqID,
			ServerUuid: serverUUID,
			Payload: &pb.NodeMessage_WriteReq{
				WriteReq: &pb.WriteFileReq{Path: filePath, TotalSize: fileHeader.Size},
			},
		}

		resp, err := h.state.GRPCRegistry.SendRequest(nodeID, writeReqMsg, 30*time.Second)
		if err != nil {
			file.Close()
			sendJSONError(w, fmt.Sprintf("Node communication error: %v", err), http.StatusBadGateway)
			return
		}
		if errResp := resp.GetError(); errResp != nil {
			file.Close()
			sendJSONError(w, errResp.Message, int(errResp.Code))
			return
		}

		// Step 2: Stream chunks directly from multipart file (no RAM buffering)
		const chunkSize = 64 * 1024
		buf := make([]byte, chunkSize)
		var offset int64

		for {
			n, readErr := file.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				chunkMsg := &pb.NodeMessage{
					RequestId:  reqID,
					ServerUuid: serverUUID,
					Payload: &pb.NodeMessage_Chunk{
						Chunk: &pb.DataChunk{Data: chunk, Offset: offset},
					},
				}
				if err := conn.Send(chunkMsg); err != nil {
					file.Close()
					sendJSONError(w, fmt.Sprintf("Failed to send chunk: %v", err), http.StatusBadGateway)
					return
				}
				offset += int64(n)
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				file.Close()
				sendJSONError(w, fmt.Sprintf("Failed to read file: %v", readErr), http.StatusInternalServerError)
				return
			}
		}
		file.Close()

		// Step 3: TransferDone
		doneMsg := &pb.NodeMessage{
			RequestId:  reqID,
			ServerUuid: serverUUID,
			Payload: &pb.NodeMessage_TransferDone{
				TransferDone: &pb.TransferDone{TotalBytes: offset, Filename: sanitizedName},
			},
		}
		if err := conn.Send(doneMsg); err != nil {
			sendJSONError(w, fmt.Sprintf("Failed to send transfer done: %v", err), http.StatusBadGateway)
			return
		}
		uploadedBytes += offset
	}

	// Count the completed upload against the user's shared daily quota, by the
	// bytes actually streamed. Runs only on full success; a mid-loop failure
	// returns above and is not counted.
	quota.RecordDailyUsage(r.Context(), h.state.Redis, username, uploadedBytes)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("%d files uploaded successfully", len(files)),
	})
}

func formatBytesHuman(b int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
