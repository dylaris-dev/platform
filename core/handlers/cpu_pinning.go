package handlers

import (
	"dylaris-core/models"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
)

// effectiveCpuset: see models.EffectiveCpuset.
var effectiveCpuset = models.EffectiveCpuset

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
	out := map[string]interface{}{
		"success":    true,
		"topology":   topo, // null when the node has not reported a topology yet
		"load":       load,
		"nodeCpuset": node.CpusetCpus, // the node's allowed core pool ("" = all cores)
	}
	// ?limit=2&server=32: the cores the allocator would give that server, so a
	// manual pin in the panel can start from the same spread auto mode uses.
	// The server's own pin is left out of the load it is placed against.
	if limit, lerr := strconv.ParseFloat(r.URL.Query().Get("limit"), 64); lerr == nil && limit > 0 && limit <= 1024 {
		exclude, _ := strconv.Atoi(r.URL.Query().Get("server"))
		if suggested, serr := h.state.CPUPinning.AutoCpuset(r.Context(), node.Token, node.ID, exclude, limit, node.CpusetCpus); serr == nil {
			out["suggested"] = suggested
		}
	}
	json.NewEncoder(w).Encode(out)
}
