package servers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/handlers"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// edgeMotdPublishTTL matches the desired-state republish cadence in the status
// watcher: the value is refreshed every scan tick (5s) with this TTL, so the
// edge always sees a live key and a deleted server's key expires on its own.
const edgeMotdPublishTTL = 60 * time.Second

// maxEdgeMotdTextLen bounds the operator custom text (a short offline notice
// rendered by the edge as an MC MOTD / disconnect message).
const maxEdgeMotdTextLen = 256

// validEdgeMotdMode reports whether mode is one of the supported modes: auto
// (built-in notice), private (built-in, but a neutral "Server Offline" instead
// of telling players the host is offline), custom (operator text), off
// (vanilla). An edge older than private reads it as auto.
func validEdgeMotdMode(mode string) bool {
	switch mode {
	case "auto", "private", "custom", "off":
		return true
	}
	return false
}

// GetServerEdgeMotd GET /api/servers/{id}/edge-motd
// Returns the per-server edge transitional-MOTD config (mode + custom text).
func (h *ServerHandler) GetServerEdgeMotd(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		handlers.SendJSONError(w, "Invalid server ID", 400)
		return
	}
	if _, err := h.state.Store.GetServerByID(serverID); err != nil {
		handlers.SendJSONError(w, "Server not found", 404)
		return
	}
	mode, text, err := h.state.Store.GetServerEdgeMotd(serverID)
	if err != nil {
		handlers.SendJSONError(w, "Failed to read edge MOTD config", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "mode": mode, "customText": text})
}

// SetServerEdgeMotd PATCH /api/servers/{id}/edge-motd
// Updates the per-server edge transitional-MOTD mode/text and publishes it to
// Redis so the gateway edge picks it up (auto/custom/off; see the edge's
// handleSpliceServerStatus). Server-scoped write (owner/granted/admin).
func (h *ServerHandler) SetServerEdgeMotd(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		handlers.SendJSONError(w, "Invalid server ID", 400)
		return
	}
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		handlers.SendJSONError(w, "Server not found", 404)
		return
	}
	var req struct {
		Mode       string `json:"mode"`
		CustomText string `json:"customText"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", 400)
		return
	}
	if !validEdgeMotdMode(req.Mode) {
		handlers.SendJSONError(w, "Invalid mode (must be auto, custom, or off)", 400)
		return
	}
	text := strings.TrimSpace(req.CustomText)
	if len(text) > maxEdgeMotdTextLen {
		text = text[:maxEdgeMotdTextLen]
	}
	if err := h.state.Store.SetServerEdgeMotd(serverID, req.Mode, text); err != nil {
		handlers.SendJSONError(w, "Failed to update edge MOTD config", 500)
		return
	}
	// Publish immediately so the edge picks it up without waiting for the periodic
	// republish (best-effort; the status watcher republishes every scan tick too).
	if h.state.Redis != nil {
		publishEdgeMotd(context.Background(), h.state.Redis, srv.UUID, req.Mode, text)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "mode": req.Mode, "customText": text})
}

// publishEdgeMotd SETs the two per-server edge-MOTD keys the gateway edge reads.
// Best-effort: a Redis error is non-fatal (the DB is the source of truth and the
// status watcher republishes on the next tick).
func publishEdgeMotd(ctx context.Context, rdb *redis.Client, uuid, mode, text string) {
	rdb.Set(ctx, fmt.Sprintf("dylaris:server:%s:edge_motd_mode", uuid), mode, edgeMotdPublishTTL)
	rdb.Set(ctx, fmt.Sprintf("dylaris:server:%s:edge_motd_text", uuid), text, edgeMotdPublishTTL)
}
