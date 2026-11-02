package servers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"dylaris-core/handlers/access"
	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/placement"
)

// PlacementHandler picks the best node for a new server based on tag,
// per-node overcommit ratios and observed allocation. The decision is
// transparent — the API returns the chosen node plus the runner-up
// scores so admins can see why.
type PlacementHandler struct {
	d Deps
}

func NewPlacementHandler(state Deps) *PlacementHandler {
	return &PlacementHandler{d: state}
}

// PickNode POST /api/placement/pick — any authed user (helper).
// Used by the deploy wizard to preview which node would be chosen and
// internally by CreateServer when the request specifies a tag instead
// of an explicit nodeId. It only returns candidate scoring, never mutates
// anything; the actual server-creation privilege is enforced separately.
func (h *PlacementHandler) PickNode(w http.ResponseWriter, r *http.Request) {
	var req placement.PickNodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	// A negative padding would shrink the request and make every node fit.
	if p := req.RAMPaddingMB; p != nil && !models.ValidRAMPaddingMB(*p) {
		httpx.SendJSONError(w, ramPaddingRefused, http.StatusBadRequest)
		return
	}

	// The SAME scope CreateServer applies, and it has to be the same one: the
	// deploy wizard calls this to show the admin which node the create is about
	// to choose ("so admins aren't deploying blind", in its own words). A
	// preview scoped differently from the placement is a preview that names a
	// machine the create will not use.
	//
	// For a tenant it also stops an empty POST from enumerating the whole
	// fleet's capacity and topology. Nothing is lost for an admin picking a node
	// by hand: that list comes from /nodes, not from here.
	applyPlacementScope(h.d, r, &req)

	resp := h.pickNode(r.Context(), req)
	json.NewEncoder(w).Encode(resp)
}

// pickNode binds placement.Pick to this handler's state.
func (h *PlacementHandler) pickNode(ctx context.Context, req placement.PickNodeRequest) placement.PickNodeResponse {
	return placement.Pick(ctx, h.d.Store, h.d.Redis, services.GatewayRoutingEnabled(h.d.Store), req)
}

// SetNodePlacementRequest is the body for PUT /api/nodes/{id}/placement.
//
// RAMPaddingMB is raw so an absent field (leave the override alone) can be
// told apart from null (clear it, inherit the global).
type SetNodePlacementRequest struct {
	CPUOvercommitRatio float64         `json:"cpuOvercommitRatio"`
	RAMOvercommitRatio float64         `json:"ramOvercommitRatio"`
	RAMPaddingMB       json.RawMessage `json:"ramPaddingMb"`
}

// SetNodePlacement PUT /api/nodes/{id}/placement — PANEL nodes.write.
// Lets admins (or a nodes.write cap holder) override the global defaults for a
// single node.
func (h *PlacementHandler) SetNodePlacement(w http.ResponseWriter, r *http.Request) {
	id := httpx.MustAtoi(extractIDFromPath(r, "/api/nodes/", "/placement"))
	if id <= 0 {
		httpx.SendJSONError(w, "Invalid node id", http.StatusBadRequest)
		return
	}
	// Overcommit on a customer's machine decides how much of their hardware the
	// scheduler hands out; that is theirs to set.
	if access.ForeignToCaller(h.d.Store, h.d.FeatureFlags, r, id) {
		httpx.SendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}
	var req SetNodePlacementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.CPUOvercommitRatio <= 0 || req.RAMOvercommitRatio <= 0 {
		httpx.SendJSONError(w, "Overcommit ratios must be > 0", http.StatusBadRequest)
		return
	}
	// Validated before either write, so a bad padding does not leave the
	// ratios saved and the request answered 400.
	paddingSet := len(req.RAMPaddingMB) > 0
	var padding *int
	if paddingSet && string(req.RAMPaddingMB) != "null" {
		var n int
		if err := json.Unmarshal(req.RAMPaddingMB, &n); err != nil || !models.ValidRAMPaddingMB(n) {
			httpx.SendJSONError(w, fmt.Sprintf("ramPaddingMb must be a whole number from 0 to %d, or null", models.MaxRAMPaddingMB), http.StatusBadRequest)
			return
		}
		padding = &n
	}
	if err := h.d.Store.SetNodePlacement(id, req.CPUOvercommitRatio, req.RAMOvercommitRatio); err != nil {
		httpx.SendJSONError(w, "Failed to update placement", http.StatusInternalServerError)
		return
	}
	if paddingSet {
		if err := h.d.Store.SetNodeRAMPadding(id, padding); err != nil {
			httpx.SendJSONError(w, "Failed to update RAM padding", http.StatusInternalServerError)
			return
		}
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// extractIDFromPath pulls an integer between a prefix and suffix in r.URL.Path.
// We use this instead of mux.Vars to avoid a route-pattern change.
func extractIDFromPath(r *http.Request, prefix, suffix string) string {
	p := r.URL.Path
	p = strings.TrimPrefix(p, prefix)
	p = strings.TrimSuffix(p, suffix)
	return p
}

// AvailableTagsHandler GET /api/placement/tags[?region=...] — returns the
// union of all tags currently advertised by online nodes, optionally scoped
// to a single region. Populates the deploy wizard's multi-select chip list.
func (h *PlacementHandler) AvailableTagsHandler(w http.ResponseWriter, r *http.Request) {
	region := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("region")))
	nodes, err := h.d.Store.ListNodes()
	if err != nil {
		httpx.SendJSONError(w, "Failed to list nodes", http.StatusInternalServerError)
		return
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		if region != "" && !strings.EqualFold(n.Region, region) {
			continue
		}
		for _, t := range strings.Split(n.Tags, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				seen[t] = true
			}
		}
	}
	tags := make([]string, 0, len(seen))
	for t := range seen {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"tags":    tags,
	})
}

// AvailableRegionsHandler GET /api/placement/regions — returns the union of
// region keys currently advertised by online nodes. The wizard hides the
// region picker when this list is empty.
func (h *PlacementHandler) AvailableRegionsHandler(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.d.Store.ListNodes()
	if err != nil {
		httpx.SendJSONError(w, "Failed to list nodes", http.StatusInternalServerError)
		return
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if n.Status != "online" || n.Region == "" {
			continue
		}
		seen[n.Region] = true
	}
	regions := make([]string, 0, len(seen))
	for r := range seen {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"regions": regions,
	})
}

// applyPlacementScope decides how an AUTOMATIC placement is scoped, which is a
// different question from "may this caller use that node". Nobody named a
// machine here, so the scheduler is about to choose one, and it must choose one
// belonging to the same party as the caller.
//
// A tenant gets their own nodes. An admin gets the platform's - not the whole
// fleet, which is what it used to be: a customer's BYON box is often the
// emptiest machine in its region, which made it the one this scheduler
// preferred, and the customer has root on it. An admin who wants that can still
// name the node; only the automatic pick is fenced.
//
// It is a function rather than two lines at the call site so a test can name
// the DECISION. Testing the scheduler with the flag already set proves the flag
// works, not that anything sets it.
func applyPlacementScope(state Deps, r *http.Request, req *placement.PickNodeRequest) {
	if !access.OwnershipInForce(state.FeatureFlags, r) {
		return // no node has an owner; nothing to scope
	}
	if reqctx.IsAdmin(r) {
		req.PlatformOnly = true
		return
	}
	uid := reqctx.ByonCallerID(r)
	req.OwnerScope = &uid
}
