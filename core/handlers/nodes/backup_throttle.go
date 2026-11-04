package nodes

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"dylaris-core/handlers/access"
	"dylaris-core/handlers/httpx"
	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// How a node's backups yield to players: its uplink, a read cap, and the
// spool. Two doors onto one handler: /api/nodes/{id}/backup-throttle for an
// operator (nodes.write, never on a customer's machine), and
// /api/me/nodes/{id}/backup-throttle for the owner of a BYON machine, whose
// uplink it is.

// GetNodeBackupThrottle GET /api/nodes/{id}/backup-throttle
func (h *NodeHandler) GetNodeBackupThrottle(w http.ResponseWriter, r *http.Request) {
	if node, ok := h.operatorNode(w, r); ok {
		h.writeBackupThrottle(w, r, node)
	}
}

// SetNodeBackupThrottle PUT /api/nodes/{id}/backup-throttle
func (h *NodeHandler) SetNodeBackupThrottle(w http.ResponseWriter, r *http.Request) {
	if node, ok := h.operatorNode(w, r); ok {
		h.saveBackupThrottle(w, r, node)
	}
}

// GetMyNodeBackupThrottle GET /api/me/nodes/{id}/backup-throttle
func (h *NodeHandler) GetMyNodeBackupThrottle(w http.ResponseWriter, r *http.Request) {
	if node, ok := h.myNode(w, r); ok {
		h.writeBackupThrottle(w, r, node)
	}
}

// SetMyNodeBackupThrottle PUT /api/me/nodes/{id}/backup-throttle
func (h *NodeHandler) SetMyNodeBackupThrottle(w http.ResponseWriter, r *http.Request) {
	if node, ok := h.myNode(w, r); ok {
		h.saveBackupThrottle(w, r, node)
	}
}

// operatorNode loads the node for the capability-gated route. A customer's
// machine answers to its owner only (CanManageNode), as storage placement does.
func (h *NodeHandler) operatorNode(w http.ResponseWriter, r *http.Request) (*models.Node, bool) {
	id, _ := strconv.Atoi(mux.Vars(r)["id"])
	node, err := h.d.Store.GetNodeByID(id)
	if err != nil || node == nil || !access.CanManageNode(h.d.FeatureFlags, r, node) {
		httpx.SendJSONError(w, "Node not found", http.StatusNotFound)
		return nil, false
	}
	return node, true
}

func (h *NodeHandler) writeBackupThrottle(w http.ResponseWriter, r *http.Request, node *models.Node) {
	json.NewEncoder(w).Encode(map[string]any{
		"success":  true,
		"throttle": services.LoadNodeBackupThrottle(h.d.Store, node.ID),
		// What the node reports itself: its env uplink and spool disk. Empty
		// for an older or offline node.
		"node": services.LoadNodeBackupEnv(r.Context(), h.d.Redis, node.Token),
	})
}

func (h *NodeHandler) saveBackupThrottle(w http.ResponseWriter, r *http.Request, node *models.Node) {
	var req services.NodeBackupThrottle
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if err := req.Validate(); err != nil {
		httpx.SendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := services.SaveNodeBackupThrottle(h.d.Store, node.ID, req); err != nil {
		httpx.SendJSONError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	// Now rather than on the next publisher tick; the tick repairs a miss.
	if err := services.PublishBackupPolicy(r.Context(), h.d.Store, h.d.Redis, *node); err != nil {
		log.Printf("backup throttle: publish for node %d: %v", node.ID, err)
	}
	h.writeBackupThrottle(w, r, node)
}
