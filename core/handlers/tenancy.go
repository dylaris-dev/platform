package handlers

import (
	"net/http"

	"dylaris-core/models"
)

// byonCallerID returns the authenticated user's id from the request context.
func byonCallerID(r *http.Request) string {
	id, _ := r.Context().Value("userID").(string)
	return id
}

// byonActive reports whether BYON multi-tenancy node-ownership scoping is on.
// When off (the default), the platform behaves as today's single-operator panel
// and NONE of the ownership scoping below applies. This is the gate that keeps
// solo / hoster behavior identical.
func byonActive(state *AppState, r *http.Request) bool {
	return state != nil && state.FeatureFlags != nil && state.FeatureFlags.IsBYONEnabled(r.Context())
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
func applyPlacementScope(state *AppState, r *http.Request, req *PickNodeRequest) {
	if !byonActive(state, r) {
		return // no node has an owner; nothing to scope
	}
	if IsAdmin(r) {
		req.PlatformOnly = true
		return
	}
	uid := byonCallerID(r)
	req.OwnerScope = &uid
}

// canManageNode reports whether the caller may manage a specific node. Admins
// (operators) always may. In BYON mode the node's owning tenant may manage their
// own node. Outside BYON mode, node management stays admin-only (today's behavior).
func canManageNode(state *AppState, r *http.Request, node *models.Node) bool {
	if IsAdmin(r) {
		return true
	}
	if byonActive(state, r) && node != nil && node.OwnerID != nil {
		uid := byonCallerID(r)
		return uid != "" && *node.OwnerID == uid
	}
	return false
}

// canPlaceOnNode reports whether the caller may place a server on a node.
//
// An OWNED node belongs to its owner, and being an admin does not change that.
// That is not a new rule here, it is the rule the rest of the platform already
// applies and this function was the one place contradicting it:
// applyPlacementScope sets PlatformOnly for an admin so auto-placement skips
// tenant machines, placement.go honours it, and the rebalance worker refuses to
// cross an ownership boundary in either direction. Only the explicit-nodeId path
// and the transfer target went through here and were waved past, so the machine
// a customer has root on was a placement target for every operator.
//
// Platform nodes (owner_id nil) stay operator-only for self-service: tenants get
// capacity on those through the rented-server path, not by placing there
// themselves. Plan-limit and node-capacity checks layer on top of this
// elsewhere.
func canPlaceOnNode(state *AppState, r *http.Request, node *models.Node) bool {
	if node == nil {
		return false
	}
	if node.OwnerID != nil && byonActive(state, r) {
		// Somebody's own hardware. Only that somebody, admin or not.
		uid := byonCallerID(r)
		return uid != "" && *node.OwnerID == uid
	}
	// Unowned, or BYON switched off so an owner_id carries no tenancy meaning
	// any more. Operator territory - and it must stay reachable by SOMEONE, or
	// turning the feature off would strand every node that still has an owner.
	return IsAdmin(r)
}
