package servers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"time"

	"dylaris-core/handlers"

	"dylaris-pkg/validate"

	"dylaris-core/models"
	"dylaris-core/store"

	pb "dylaris-proto/node"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// disabledModDir is where a switched-off jar lives, inside its mods/ or
// plugins/ directory. No loader reads it: Fabric, NeoForge, Forge, Paper,
// Bukkit, Velocity and BungeeCord list one directory level, and Quilt, the one
// that recurses, skips dot-folders by name.
const disabledModDir = ".disabled"

// modPaths is where a mod's jar is when enabled and when disabled, relative to
// the server root as the node's file operations expect.
func modPaths(srv *models.Server, m *models.ServerMod) (enabled, disabled, disabledDir string) {
	dir := path.Join(srv.ActiveSubServer, uninstallTargetDir(m, srv.InstallerType))
	return path.Join(dir, m.FileName), path.Join(dir, disabledModDir, m.FileName), path.Join(dir, disabledModDir)
}

// SetEnabled PUT /api/servers/{id}/mods/{modId}/enabled - switches one mod on
// or off by moving its jar between <dir>/ and <dir>/.disabled/. Body
// {"enabled": bool}. Takes effect at the next start.
//
// Built from the node's existing create / copy / delete file operations, so it
// works on every node version without a node update. Copy before delete: a
// failure in between leaves the jar in both places (reported, and repeatable),
// never in neither.
func (h *ServerModsHandler) SetEnabled(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		handlers.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	if handlers.RefuseIfSuspended(w, r, h.state, srv) {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil || req.Enabled == nil {
		handlers.SendJSONError(w, "enabled (true or false) is required", http.StatusBadRequest)
		return
	}
	modID, _ := strconv.Atoi(mux.Vars(r)["modId"])
	mods, err := h.state.Store.ListServerMods(serverID, srv.ActiveSubServer)
	if err != nil {
		handlers.SendJSONError(w, "Failed to list mods", http.StatusInternalServerError)
		return
	}
	var target *models.ServerMod
	for i := range mods {
		if mods[i].ID == modID {
			target = &mods[i]
		}
	}
	if target == nil {
		handlers.SendJSONError(w, "Mod not found", http.StatusNotFound)
		return
	}
	if !validate.IsPlainFileName(target.FileName) {
		handlers.SendJSONError(w, "This mod's file name cannot be moved safely; remove and reinstall it.", http.StatusConflict)
		return
	}
	if target.Status == models.ServerModInstalling {
		handlers.SendJSONError(w, "This mod is still being installed.", http.StatusConflict)
		return
	}
	enable := *req.Enabled
	if target.Disabled == !enable {
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	if h.state.GRPCRegistry == nil {
		handlers.SendJSONError(w, "Node unavailable", http.StatusServiceUnavailable)
		return
	}
	on, off, offDir := modPaths(srv, target)
	src, dst := on, off
	if enable {
		src, dst = off, on
	}
	if !enable {
		// Already there is fine; anything else surfaces at the copy.
		_ = h.nodeFileOp(srv, &pb.NodeMessage_CreateReq{CreateReq: &pb.CreateFileReq{Path: offDir, IsDir: true}})
	}
	if err := h.nodeFileOp(srv, &pb.NodeMessage_CopyReq{CopyReq: &pb.CopyFileReq{SrcPath: src, DstPath: dst}}); err != nil {
		handlers.SendJSONError(w, fmt.Sprintf("Could not move %s: %v", target.FileName, err), http.StatusBadGateway)
		return
	}
	if err := h.nodeFileOp(srv, &pb.NodeMessage_DeleteReq{DeleteReq: &pb.DeleteFileReq{Path: src}}); err != nil {
		handlers.SendJSONError(w, fmt.Sprintf("%s was copied but the old file could not be removed: %v. Try again.", target.FileName, err), http.StatusBadGateway)
		return
	}
	if err := h.state.Store.SetServerModDisabled(target.ID, serverID, !enable); err != nil {
		if errors.Is(err, store.ErrServerModNotFound) {
			handlers.SendJSONError(w, "Mod not found", http.StatusNotFound)
			return
		}
		handlers.SendJSONError(w, "The jar moved, but the setting could not be saved", http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "server_mods.changed", map[string]interface{}{"serverId": serverID})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// nodeFileOp runs one file operation on the server's node and turns a node
// error into a Go error.
func (h *ServerModsHandler) nodeFileOp(srv *models.Server, payload any) error {
	msg := &pb.NodeMessage{RequestId: uuid.NewString(), ServerUuid: srv.UUID}
	switch p := payload.(type) {
	case *pb.NodeMessage_CreateReq:
		msg.Payload = p
	case *pb.NodeMessage_CopyReq:
		msg.Payload = p
	case *pb.NodeMessage_DeleteReq:
		msg.Payload = p
	default:
		return fmt.Errorf("unsupported file operation %T", payload)
	}
	resp, err := h.state.GRPCRegistry.SendRequest(srv.NodeID, msg, 30*time.Second)
	if err != nil {
		return err
	}
	if e := resp.GetError(); e != nil {
		return errors.New(e.Message)
	}
	return nil
}
