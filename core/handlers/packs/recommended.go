package packs

import (
	"dylaris-core/handlers"
	"encoding/json"
	"github.com/gorilla/mux"
	"net/http"
	"strconv"
)

// SetRecommended changes guidance and alert input; it does not publish, freeze
// a build or modify a running server installation.
func (h *PacksHandler) SetRecommended(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(mux.Vars(r)["id"])
	p, ok := h.packAccess(r, id, packEdit)
	if !ok {
		handlers.SendJSONError(w, "Forbidden", 403)
		return
	}
	var req struct {
		BuildID *int `json:"buildId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil || req.BuildID == nil || *req.BuildID < 0 {
		handlers.SendJSONError(w, "Choose a build or automatic latest release", 400)
		return
	}
	if err := h.state.Store.SetPackRecommended(r.Context(), p.ID, *req.BuildID); err != nil {
		handlers.SendJSONError(w, "Could not set recommended build; it must belong to this pack", 409)
		return
	}
	h.audit(r, p.OwnerID, p.ID, "pack.recommended_changed", map[string]any{"buildId": *req.BuildID})
	publishPackEvent(r, h.state, "pack_builds.changed", p.ID, map[string]interface{}{"packId": p.ID})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
