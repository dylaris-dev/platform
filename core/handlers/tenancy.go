package handlers

import (
	"context"
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

// routeOnlyActive reports whether route-only (tenant link kits) is on. See
// FeatureFlags.IsRouteOnlyEnabled for why it follows BYON while unset.
func routeOnlyActive(state *AppState, r *http.Request) bool {
	return state != nil && state.FeatureFlags != nil && state.FeatureFlags.IsRouteOnlyEnabled(r.Context())
}

// ownershipInForce is byonActive for the ownership fences below, failing closed:
// a flag that cannot be read keeps a customer's machine theirs. byonActive stays
// for the gates that OPEN tenant features, where failing closed would do the
// opposite. The two agree whenever the settings table answers.
func ownershipInForce(state *AppState, r *http.Request) bool {
	if state == nil || state.FeatureFlags == nil {
		return false // no flags wired: a bare test or tool state, BYON cannot be on
	}
	return state.FeatureFlags.BYONOwnershipInForce(r.Context())
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
	if !ownershipInForce(state, r) {
		return // no node has an owner; nothing to scope
	}
	if IsAdmin(r) {
		req.PlatformOnly = true
		return
	}
	uid := byonCallerID(r)
	req.OwnerScope = &uid
}

// ownedByOther reports whether node is somebody's own hardware and the caller is
// not that somebody, while ownership is in force. It is the one question every
// read of a machine's CONTENTS has to ask before anything else, admin or not.
//
// With BYON off an owner_id carries no tenancy meaning (see canPlaceOnNode), so
// nothing is anybody else's and this answers false.
func ownedByOther(state *AppState, r *http.Request, node *models.Node) bool {
	if node == nil || node.OwnerID == nil || !ownershipInForce(state, r) {
		return false
	}
	uid := byonCallerID(r)
	return uid == "" || *node.OwnerID != uid
}

// NodeOwnedByOther is ownedByOther by node id and user, for the resolver, which
// has no request. main installs it with SetForeignNode.
//
// A node it cannot read counts as foreign, and so does a flag (see
// ownershipInForce): the cost is an admin refused during a database fault, not
// a customer's world handed to every operator. The flag is only asked once the
// node turned out to have an owner, which platform servers never do.
func (s *AppState) NodeOwnedByOther(nodeID int, userID string) bool {
	if s == nil || s.Store == nil {
		return true
	}
	node, err := s.Store.GetNodeByID(nodeID)
	if err != nil || node == nil {
		return true
	}
	return s.userOwnedByOther(node.OwnerID, userID)
}

// userOwnedByOther is the same question for anything that carries its owner's
// id directly, such as a warp node key or link kit: owned by somebody who is not
// userID, while ownership is in force. nil or empty means the platform's.
func (s *AppState) userOwnedByOther(ownerID *string, userID string) bool {
	if ownerID == nil || *ownerID == "" || (userID != "" && *ownerID == userID) {
		return false
	}
	if s.FeatureFlags == nil {
		return true
	}
	return s.FeatureFlags.BYONOwnershipInForce(context.Background())
}

// kitOwnedByOther is userOwnedByOther for a route-only link kit, whose owner is
// fenced whenever route-only is on, not only with BYON.
func (s *AppState) kitOwnedByOther(ownerID string, userID string) bool {
	if ownerID == "" || (userID != "" && ownerID == userID) {
		return false
	}
	if s.FeatureFlags == nil {
		return true
	}
	return s.FeatureFlags.RouteOnlyOwnershipInForce(context.Background())
}

// foreignToCaller is NodeOwnedByOther for the caller of r. It guards the admin
// and staff WRITES on a machine or on a server by the machine it runs on - move,
// reassign, re-pair, CPU pool, overcommit, demo flag - which are capability-gated
// and so fleet-wide by construction. Callers answer with "not found", like
// diskToolNode, so the refusal does not confirm what exists.
func foreignToCaller(state *AppState, r *http.Request, nodeID int) bool {
	return state.NodeOwnedByOther(nodeID, byonCallerID(r))
}

// canManageNode reports whether the caller may read or configure what is ON a
// node: its servers, its storage and storage placement, its deploy bundle, its
// CPU topology.
//
// An OWNED node answers to its owner only, and being an admin does not change
// that - the same rule canPlaceOnNode applies, for the same reason. It used to
// short-circuit on admin first, so GET /api/nodes/{id}/servers handed every
// operator the server list of a customer's machine while ListServersForUser
// deliberately withheld the very same rows ("An operator runs the platform; they
// do not run their customers' machines."). Two answers to one question.
//
// The other doors onto a customer's machine ask foreignToCaller or the resolver
// (SetForeignNode) instead: opening its servers, their files and beam, reset
// pairing and roll key, CPU pool and overcommit, moving, transferring and
// reassigning its servers, the demo flag. Decommissioning (DELETE
// /api/nodes/{id}) goes through none of them, and that one is meant to stay open.
func canManageNode(state *AppState, r *http.Request, node *models.Node) bool {
	if node != nil && node.OwnerID != nil && ownershipInForce(state, r) {
		return !ownedByOther(state, r, node)
	}
	return IsAdmin(r)
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
	if node.OwnerID != nil && ownershipInForce(state, r) {
		// Somebody's own hardware. Only that somebody, admin or not.
		uid := byonCallerID(r)
		return uid != "" && *node.OwnerID == uid
	}
	// Unowned, or BYON switched off so an owner_id carries no tenancy meaning
	// any more. Operator territory - and it must stay reachable by SOMEONE, or
	// turning the feature off would strand every node that still has an owner.
	return IsAdmin(r)
}
