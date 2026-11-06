package servers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"dylaris-core/handlers/guard"
	"dylaris-core/handlers/httpx"

	"github.com/gorilla/mux"
)

// stripStorageServerUUIDs removes the per-path server_uuids list from a decoded
// heartbeat storage payload. The payload is whatever the node sent, so anything
// that is not the expected shape is returned untouched EXCEPT that a non-list
// or a non-object entry cannot carry the key in the first place.
func stripStorageServerUUIDs(v interface{}) interface{} {
	entries, ok := v.([]interface{})
	if !ok {
		return v
	}
	for _, e := range entries {
		if m, ok := e.(map[string]interface{}); ok {
			delete(m, "server_uuids")
		}
	}
	return entries
}

// GetServerStoragePath returns the current storage path for a server and all available
// storage paths on its node (from the node's Redis heartbeat).
// GET /api/servers/{id}/storage-path  (gated by RequireCap("server.settings.write") at the route)
//
// The answer is NODE-wide, not server-wide: every storage path configured on
// the machine with its capacity and how many servers sit on each. It exists to
// feed the migrate-storage picker, whose action takes server.settings.write, so
// the read takes the same cap rather than overview.read - the cap every invite
// carries, which had a viewer on one server reading the operator's disk layout.
//
// The heartbeat entries also carry server_uuids, the top-level directories on
// each path, i.e. the identifiers of every OTHER tenant's servers on that node.
// Nothing renders it (the panel's StoragePathInfo has no such field), so it is
// stripped here rather than merely re-gated: on a shared node, no per-server
// endpoint should enumerate co-tenants for anyone, owner included.
func (h *ServerHandler) GetServerStoragePath(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		httpx.SendJSONError(w, "Invalid server ID", 400)
		return
	}

	srv, err := h.d.Store.GetServerByID(serverID)
	if err != nil || srv == nil {
		httpx.SendJSONError(w, "Server not found", 404)
		return
	}

	node, err := h.d.Store.GetNodeByID(srv.NodeID)
	if err != nil || node == nil {
		httpx.SendJSONError(w, "Node not found", 404)
		return
	}

	// Current storage path from Redis
	currentPath := ""
	if h.d.Redis != nil {
		key := fmt.Sprintf("node:%s:server:%s:storage", node.Token, srv.UUID)
		if val, redisErr := h.d.Redis.Get(r.Context(), key).Result(); redisErr == nil {
			currentPath = val
		}
	}

	// Available storage paths from node heartbeat
	var storagePaths interface{} = []interface{}{}
	if h.d.Redis != nil {
		heartbeatKey := "dylaris:discovery:" + node.Token
		if val, redisErr := h.d.Redis.Get(r.Context(), heartbeatKey).Result(); redisErr == nil {
			var heartbeat map[string]interface{}
			if json.Unmarshal([]byte(val), &heartbeat) == nil {
				if s, ok := heartbeat["storage"]; ok {
					storagePaths = stripStorageServerUUIDs(s)
				}
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":      true,
		"currentPath":  currentPath,
		"storagePaths": storagePaths,
	})
}

// MigrateServerStorage queues a migrate_storage command on the server's node.
// POST /api/servers/{id}/migrate-storage  (gated by RequireCap("server.settings.write") at the route)
func (h *ServerHandler) MigrateServerStorage(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		httpx.SendJSONError(w, "Invalid server ID", 400)
		return
	}

	var req struct {
		TargetPath string `json:"targetPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TargetPath == "" {
		httpx.SendJSONError(w, "targetPath is required", 400)
		return
	}

	srv, err := h.d.Store.GetServerByID(serverID)
	if err != nil || srv == nil {
		httpx.SendJSONError(w, "Server not found", 404)
		return
	}

	if guard.RefuseIfSuspended(w, r, h.d.Store, h.d.SuspendGrace, srv) {
		return
	}
	// The move copies the tree away and deletes it: under an install it pulls
	// the directory out from under it, and under a move between nodes the
	// staged archive stays on the old path, where nothing cleans it up.
	switch srv.Status {
	case "installing":
		httpx.SendJSONError(w, "The server is installing. Move its storage when the install has finished.", http.StatusConflict)
		return
	case "migrating":
		httpx.SendJSONError(w, "The server is being moved to another node. Move its storage when the move has finished.", http.StatusConflict)
		return
	}

	node, err := h.d.Store.GetNodeByID(srv.NodeID)
	if err != nil || node == nil {
		httpx.SendJSONError(w, "Node not found", 404)
		return
	}

	if h.d.Queue == nil {
		httpx.SendJSONError(w, "Queue not available", 503)
		return
	}

	if err := h.d.Queue.SendMigrateCommand(r.Context(), node.Token, srv.UUID, req.TargetPath); err != nil {
		log.Printf("Failed to queue migrate_storage for server %d: %v", serverID, err)
		httpx.SendJSONError(w, "Failed to queue migration", 500)
		return
	}

	log.Printf("migrate_storage queued for server %d (%s) → %s", serverID, srv.UUID, req.TargetPath)
	h.d.Events.Publish(r.Context(), "servers.changed", nil)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Migration queued. Server will be stopped and data moved to the new path.",
	})
}
