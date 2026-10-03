package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"
)

// PlansHandler sets a tenant's per-user LIMIT OVERRIDES. Admin-only, and 0 is
// interpreted as unlimited.
//
// It used to carry CRUD for plan tiers and plan assignment as well. Those are
// gone: the hosted store never sold a plan - it pushes a node COUNT through this
// same override path - and letting self-hosters define tariffs of their own was
// a product nobody asked for. The overrides are what both paths actually used.
//
// The name is kept because the capability that guards it is still plans.write,
// which is in the authz catalogue and in deployed role assignments.
type PlansHandler struct {
	state *AppState
}

func NewPlansHandler(state *AppState) *PlansHandler { return &PlansHandler{state: state} }

func (h *PlansHandler) SetUserLimitOverrides(w http.ResponseWriter, r *http.Request) {
	userID := mux.Vars(r)["id"]
	if !guardAccountTerms(w, r, h.state, userID) {
		return
	}
	var req struct {
		MaxNodes          *int64 `json:"maxNodes"`
		MaxLinks          *int64 `json:"maxLinks"`
		TrafficEdgeGb     *int64 `json:"trafficEdgeGb"`
		TrafficRelayGb    *int64 `json:"trafficRelayGb"`
		TrafficCombinedGb *int64 `json:"trafficCombinedGb"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	for _, v := range []*int64{req.MaxNodes, req.MaxLinks, req.TrafficEdgeGb, req.TrafficRelayGb, req.TrafficCombinedGb} {
		if v != nil && *v < 0 {
			sendJSONError(w, "Limit overrides must be >= 0 (0 means none; omit the field for no limit)", http.StatusBadRequest)
			return
		}
	}
	if err := h.state.Store.SetUserLimitOverrides(userID, req.MaxNodes, req.MaxLinks, req.TrafficEdgeGb, req.TrafficRelayGb, req.TrafficCombinedGb); err != nil {
		sendJSONError(w, "Failed to save overrides", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
