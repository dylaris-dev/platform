package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"

	"dylaris-core/services"
)

// PlansHandler sets a tenant's per-user LIMIT OVERRIDES. Admin-only. The node
// and link ceilings are an operator's layer over the purchase; the traffic
// thresholds follow the platform convention (null none, 0 none allowed, n).
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
		// null follows the purchase, "unlimited" lifts the ceiling, a number
		// replaces it. Never access: that is a grant.
		MaxNodes          json.RawMessage `json:"maxNodes"`
		MaxLinks          json.RawMessage `json:"maxLinks"`
		TrafficEdgeGb     *int64          `json:"trafficEdgeGb"`
		TrafficRelayGb    *int64          `json:"trafficRelayGb"`
		TrafficCombinedGb *int64          `json:"trafficCombinedGb"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	for _, v := range []*int64{req.TrafficEdgeGb, req.TrafficRelayGb, req.TrafficCombinedGb} {
		if v != nil && *v < 0 {
			sendJSONError(w, "Limit overrides must be >= 0 (0 means none; omit the field for no limit)", http.StatusBadRequest)
			return
		}
	}
	nodes, nerr := parseAdminCeiling(req.MaxNodes)
	links, lerr := parseAdminCeiling(req.MaxLinks)
	if nerr != nil || lerr != nil {
		sendJSONError(w, `Node and link ceilings are null (follow the purchase), "unlimited" or a number >= 0`, http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetUserLimitOverrides(userID, nodes, links, req.TrafficEdgeGb, req.TrafficRelayGb, req.TrafficCombinedGb); err != nil {
		sendJSONError(w, "Failed to save overrides", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// parseAdminCeiling turns the PATCH value into the stored sentinel: "" follows
// the purchase, "unlimited" or a number replaces it.
func parseAdminCeiling(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var word string
	if json.Unmarshal(raw, &word) == nil {
		if word == services.LimitUnlimited {
			return word, nil
		}
		return "", errors.New("unknown ceiling")
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
		return "", errors.New("invalid ceiling")
	}
	return strconv.FormatInt(n, 10), nil
}
