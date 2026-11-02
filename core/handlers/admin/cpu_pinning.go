package admin

import (
	"dylaris-core/handlers/access"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"

	"dylaris-core/handlers/httpx"
)

// CPUPinningHandler serves node CPU topology for the pinning UI.
type CPUPinningHandler struct {
	d Deps
}

func NewCPUPinningHandler(d Deps) *CPUPinningHandler {
	return &CPUPinningHandler{d: d}
}

// GetNodeCPU GET /api/nodes/{id}/cpu - the host CPU topology a node reported plus
// the per-core pinning load (how many servers are pinned to each core). Gated to
// admins and users allowed to change resources, since that is who configures pinning.
func (h *CPUPinningHandler) GetNodeCPU(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		httpx.SendJSONError(w, "Invalid node ID", http.StatusBadRequest)
		return
	}
	node, err := h.d.Store.GetNodeByID(id)
	if err != nil {
		httpx.SendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}
	// Allowed: the node's owner, an admin on a platform node, or a user whose
	// CanChangeResources flag is set - on a node that is not somebody else's.
	// That flag is per user (see ComputeEffectivePermissions) and every admin
	// holds it too, so without the ownedByOther fence the admin would walk back
	// in through this second arm exactly where canManageNode now refuses.
	userID, _ := r.Context().Value("userID").(string)
	perms := access.LoadEffectivePermissions(h.d.Store, h.d.Authz, userID)
	staffMay := perms.CanChangeResources && !access.OwnedByOther(h.d.FeatureFlags, r, node)
	if !access.CanManageNode(h.d.FeatureFlags, r, node) && !staffMay {
		httpx.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	if h.d.CPUPinning == nil {
		httpx.SendJSONError(w, "CPU pinning unavailable", http.StatusServiceUnavailable)
		return
	}

	topo, _ := h.d.CPUPinning.GetNodeTopology(r.Context(), node.Token)
	load, _ := h.d.CPUPinning.NodeCoreLoad(node.ID, 0)
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
		if suggested, serr := h.d.CPUPinning.AutoCpuset(r.Context(), node.Token, node.ID, exclude, limit, node.CpusetCpus); serr == nil {
			out["suggested"] = suggested
		}
	}
	json.NewEncoder(w).Encode(out)
}
