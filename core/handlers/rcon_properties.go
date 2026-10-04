package handlers

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"dylaris-core/models"
	pb "dylaris-proto/node"

	"github.com/google/uuid"
)

// serverPropertiesName is the MC config file, relative to the active
// sub-server directory (the node joins it onto <storage>/<uuid>/<sub>/).
const serverPropertiesName = "server.properties"

// mergeServerProperties returns the server.properties content with each key in
// kv set to its value, preserving every other line, comment, and the existing
// ordering. A key already present has only its value replaced; a key not
// present is appended (appended keys are emitted in sorted order for a
// deterministic result). The Minecraft server.properties format is a flat
// key=value file (one pair per line, '#' or '!' introduces a comment), which
// is what this parses - no line continuations, no escaping.
//
// This is how RCON config reaches the actual server: Core owns the DB flags,
// but MC only opens the RCON port if enable-rcon/rcon.port/rcon.password live
// in server.properties, so on enable we merge them in and the file (durable in
// the server directory) keeps them across restarts.
func mergeServerProperties(existing string, kv map[string]string) string {
	remaining := make(map[string]string, len(kv))
	for k, v := range kv {
		remaining[k] = v
	}

	// Normalize CRLF so we split cleanly; MC writes LF.
	lines := strings.Split(strings.ReplaceAll(existing, "\r\n", "\n"), "\n")

	// A trailing newline yields a final empty element; drop it so we don't
	// grow a blank line on every write, then re-add exactly one at the end.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	out := make([]string, 0, len(lines)+len(kv))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "!") {
			out = append(out, line)
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			out = append(out, line)
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if newVal, ok := remaining[key]; ok {
			out = append(out, key+"="+newVal)
			delete(remaining, key)
			continue
		}
		out = append(out, line)
	}

	appendKeys := make([]string, 0, len(remaining))
	for k := range remaining {
		appendKeys = append(appendKeys, k)
	}
	sort.Strings(appendKeys)
	for _, k := range appendKeys {
		out = append(out, k+"="+remaining[k])
	}

	return strings.Join(out, "\n") + "\n"
}

// rconNeedsStamping reports whether a sub-server that is about to become the
// running one should have the stored RCON config written into its
// server.properties first.
//
// The reason this is needed at all: rcon_enabled/port/password are ONE row per
// SERVER, but enable-rcon lives in EACH SUB-SERVER's server.properties, and
// SetConfig only ever writes the sub-server that was active when it ran. So
// switching sub-servers (or starting one installed later) left the file behind
// while Core went on reporting RCON as enabled with nothing pending - the panel
// unlocked all four Players tabs on that word and every action then failed with
// a raw dial error. Found live: DB said enabled/no-restart, creative's file said
// enable-rcon=false, survival's said true.
//
// The password check is what keeps this from stamping three keys onto every
// server on the platform at every start: a server that has never configured
// RCON has no password, and its file is left alone.
func rconNeedsStamping(subServer string, enabled bool, password string) bool {
	if subServer == "" {
		return false
	}
	return enabled || password != ""
}

// syncRconToSubServer stamps the server's stored RCON config into subServer's
// server.properties, so the file agrees with what Core reports and the Players
// tabs act on. Returns whether the sub-server can be considered in step: true
// also when there was nothing to stamp, false only when a write was needed and
// did not land.
//
// Best-effort by design: it runs on the (re)start and switch paths, and a node
// that cannot be written to must not stop a server from starting. What it must
// not do is let the caller then report the config as applied - hence the bool.
func (h *ServerHandler) syncRconToSubServer(srv *models.Server, subServer string) bool {
	// Ahead of the DB read, not folded into rconNeedsStamping below: a power
	// action on a server that was never installed has no properties file to
	// write, and no reason to ask the database about one.
	if subServer == "" {
		return true
	}
	if h.state == nil || h.state.Store == nil {
		return false
	}
	enabled, port, password, err := h.state.Store.GetServerRconConfig(srv.ID)
	if err != nil {
		log.Printf("rcon sync for server %d: reading the stored config failed: %v", srv.ID, err)
		return false
	}
	if !rconNeedsStamping(subServer, enabled, password) {
		return true
	}
	if err := NewRconHandler(h.state).applyProps(srv.NodeID, srv.UUID, subServer, enabled, port, password); err != nil {
		log.Printf("rcon sync for server %d (%s): writing server.properties failed: %v", srv.ID, subServer, err)
		return false
	}
	return true
}

// applyRconToServerProperties merges the RCON settings into the active
// sub-server's server.properties on the node, so MC actually opens (or, when
// disabled, stops re-opening on next launch) the RCON port. Read-modify-write
// over the gRPC file API: read the current file, set the three rcon keys, write
// it back.
//
// Safety: a genuinely missing file (node returns code 404) means the server has
// never generated server.properties yet; we then write a fresh 3-key file and
// MC fills the rest on first boot. Any OTHER read error aborts WITHOUT writing,
// so a transient node/storage failure can never clobber an existing file (world
// settings, motd, ...) with a 3-key stub. The caller must not flip the DB flag
// unless this returns nil, keeping the DB and server.properties consistent.
func (h *RconHandler) applyRconToServerProperties(nodeID int, serverUUID, activeSubServer string, enabled bool, port int, password string) error {
	if activeSubServer == "" {
		return fmt.Errorf("server has no active sub-server yet - install or start it before configuring RCON")
	}
	if port == 0 {
		port = defaultRconPort
	}
	relPath := activeSubServer + "/" + serverPropertiesName

	existing, found, err := h.readNodeFileString(nodeID, serverUUID, relPath)
	if err != nil {
		return err
	}
	if !found {
		existing = ""
	}

	enabledStr := "false"
	if enabled {
		enabledStr = "true"
	}
	merged := mergeServerProperties(existing, map[string]string{
		"enable-rcon":   enabledStr,
		"rcon.port":     strconv.Itoa(port),
		"rcon.password": password,
	})

	return h.writeNodeFileString(nodeID, serverUUID, relPath, merged)
}

// readNodeFileString reads a whole (small) file from the node over the gRPC
// file API and returns its content. found=false with a nil error means the node
// reported the file does not exist (code 404); any other failure is a real
// error. Mirrors the streaming-read collection in FileHandler.GetFileContentHandler.
func (h *RconHandler) readNodeFileString(nodeID int, serverUUID, relPath string) (string, bool, error) {
	if h.state.GRPCRegistry == nil {
		return "", false, fmt.Errorf("node registry not available")
	}
	reqID := uuid.NewString()
	msg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_ReadReq{ReadReq: &pb.ReadFileReq{Path: relPath, ZipIfDir: false}},
	}
	ch, err := h.state.GRPCRegistry.SendRequestStreaming(nodeID, msg)
	if err != nil {
		return "", false, fmt.Errorf("node read failed: %w", err)
	}
	defer h.state.GRPCRegistry.CleanupRequest(nodeID, reqID)

	// server.properties is a few KB; the bound only stops a path that is not.
	data, errResp, rerr := collectNodeFile(ch, 1<<20)
	if errResp != nil {
		if errResp.Code == 404 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("node read error: %s", errResp.Message)
	}
	if rerr != nil {
		return "", false, rerr
	}
	return string(data), true, nil
}

// writeNodeFileString writes content to a node file over the gRPC file API
// (WriteFileReq -> DataChunk* -> TransferDone). Mirrors FileHandler.SaveFileHandler.
func (h *RconHandler) writeNodeFileString(nodeID int, serverUUID, relPath, content string) error {
	if h.state.GRPCRegistry == nil {
		return fmt.Errorf("node registry not available")
	}
	conn, ok := h.state.GRPCRegistry.GetConnection(nodeID)
	if !ok {
		return fmt.Errorf("node %d not connected", nodeID)
	}

	reqID := uuid.NewString()
	data := []byte(content)

	writeReqMsg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_WriteReq{WriteReq: &pb.WriteFileReq{Path: relPath, TotalSize: int64(len(data))}},
	}
	resp, err := h.state.GRPCRegistry.SendRequest(nodeID, writeReqMsg, 30*time.Second)
	if err != nil {
		return fmt.Errorf("node write init failed: %w", err)
	}
	if errResp := resp.GetError(); errResp != nil {
		return fmt.Errorf("node write error: %s", errResp.Message)
	}
	wait, stop := conn.AwaitReply(reqID)
	defer stop()

	const chunkSize = 64 * 1024
	for offset := 0; offset < len(data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunkMsg := &pb.NodeMessage{
			RequestId:  reqID,
			ServerUuid: serverUUID,
			Payload:    &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: data[offset:end], Offset: int64(offset)}},
		}
		if err := conn.Send(chunkMsg); err != nil {
			return fmt.Errorf("node write chunk failed: %w", err)
		}
	}

	doneMsg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{TotalBytes: int64(len(data))}},
	}
	if err := conn.Send(doneMsg); err != nil {
		return fmt.Errorf("node write finalize failed: %w", err)
	}
	if _, msg, ok := confirmNodeWrite(wait); !ok {
		return fmt.Errorf("node write failed: %s", msg)
	}
	return nil
}
