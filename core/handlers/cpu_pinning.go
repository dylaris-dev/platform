package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
)

// effectiveCpuset is the cpuset Core sends to the node for a server: the server's
// own pinned cpuset (auto/manual) when set, otherwise the node's static default.
// This keeps 'shared'/unpinned servers on the node default (prior behavior) while
// preserving a server's pinning across every recreate path (setup, switch,
// resource change).
func effectiveCpuset(mode, serverCpuset, nodeCpuset string) string {
	if (mode == "auto" || mode == "manual") && serverCpuset != "" {
		return serverCpuset
	}
	return nodeCpuset
}

// CPUPinningHandler serves node CPU topology for the pinning UI.
type CPUPinningHandler struct {
	state *AppState
}

func NewCPUPinningHandler(state *AppState) *CPUPinningHandler {
	return &CPUPinningHandler{state: state}
}

// GetNodeCPU GET /api/nodes/{id}/cpu - the host CPU topology a node reported plus
// the per-core pinning load (how many servers are pinned to each core). Gated to
// admins and users allowed to change resources, since that is who configures pinning.
func (h *CPUPinningHandler) GetNodeCPU(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid node ID", http.StatusBadRequest)
		return
	}
	node, err := h.state.Store.GetNodeByID(id)
	if err != nil {
		sendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}
	// Allowed: the node's owner, an admin on a platform node, or a user whose
	// CanChangeResources flag is set - on a node that is not somebody else's.
	// That flag is per user (see ComputeEffectivePermissions) and every admin
	// holds it too, so without the ownedByOther fence the admin would walk back
	// in through this second arm exactly where canManageNode now refuses.
	userID, _ := r.Context().Value("userID").(string)
	perms := LoadEffectivePermissions(h.state, userID)
	staffMay := perms.CanChangeResources && !ownedByOther(h.state, r, node)
	if !canManageNode(h.state, r, node) && !staffMay {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	if h.state.CPUPinning == nil {
		sendJSONError(w, "CPU pinning unavailable", http.StatusServiceUnavailable)
		return
	}

	topo, _ := h.state.CPUPinning.GetNodeTopology(r.Context(), node.Token)
	load, _ := h.state.CPUPinning.NodeCoreLoad(node.ID, 0)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"topology":   topo, // null when the node has not reported a topology yet
		"load":       load,
		"nodeCpuset": node.CpusetCpus, // the node's allowed core pool ("" = all cores)
	})
}
