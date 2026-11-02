package access

import (
	"context"
	"net/http"

	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// ByonActive reports whether BYON multi-tenancy node-ownership scoping is on.
// When off (the default), the platform behaves as today's single-operator panel
// and NONE of the ownership scoping below applies. This is the gate that keeps
// solo / hoster behavior identical.
func ByonActive(flags *services.FeatureFlags, r *http.Request) bool {
	return flags != nil && flags.IsBYONEnabled(r.Context())
}

// RouteOnlyActive reports whether route-only (tenant link kits) is on. See
// FeatureFlags.IsRouteOnlyEnabled for why it follows BYON while unset.
func RouteOnlyActive(flags *services.FeatureFlags, r *http.Request) bool {
	return flags != nil && flags.IsRouteOnlyEnabled(r.Context())
}

// OwnershipInForce is ByonActive for the ownership fences below, failing closed:
// a flag that cannot be read keeps a customer's machine theirs. ByonActive stays
// for the gates that OPEN tenant features, where failing closed would do the
// opposite. The two agree whenever the settings table answers.
func OwnershipInForce(flags *services.FeatureFlags, r *http.Request) bool {
	if flags == nil {
		return false // no flags wired: a bare test or tool state, BYON cannot be on
	}
	return flags.BYONOwnershipInForce(r.Context())
}

// OwnedByOther reports whether node is somebody's own hardware and the caller is
// not that somebody, while ownership is in force. It is the one question every
// read of a machine's CONTENTS has to ask before anything else, admin or not.
//
// With BYON off an owner_id carries no tenancy meaning (see CanPlaceOnNode), so
// nothing is anybody else's and this answers false.
func OwnedByOther(flags *services.FeatureFlags, r *http.Request, node *models.Node) bool {
	if node == nil || node.OwnerID == nil || !OwnershipInForce(flags, r) {
		return false
	}
	uid := reqctx.ByonCallerID(r)
	return uid == "" || *node.OwnerID != uid
}

// NodeOwnedByOther is OwnedByOther by node id and user, for the resolver, which
// has no request. main installs ServerNodeOwnedByOther, which asks this when
// the server row did not carry its node's owner.
//
// A node it cannot read counts as foreign, and so does a flag (see
// OwnershipInForce): the cost is an admin refused during a database fault, not
// a customer's world handed to every operator. The flag is only asked once the
// node turned out to have an owner, which platform servers never do.
func NodeOwnedByOther(st store.Store, flags *services.FeatureFlags, nodeID int, userID string) bool {
	if st == nil {
		return true
	}
	node, err := st.GetNodeByID(nodeID)
	if err != nil || node == nil {
		return true
	}
	return UserOwnedByOther(flags, node.OwnerID, userID)
}

// ServerNodeOwnedByOther is NodeOwnedByOther for a server row, main's
// SetForeignNode predicate. A row from GetServerByID/GetServerByUUID carries
// its node's owner from the same query (NodeOwnerLoaded), so the node is not
// read a second time; any other row falls back to reading the node.
func ServerNodeOwnedByOther(st store.Store, flags *services.FeatureFlags, srv *models.Server, userID string) bool {
	if srv == nil {
		return true
	}
	if srv.NodeOwnerLoaded {
		return UserOwnedByOther(flags, srv.NodeOwnerID, userID)
	}
	return NodeOwnedByOther(st, flags, srv.NodeID, userID)
}

// UserOwnedByOther is the same question for anything that carries its owner's
// id directly, such as a warp node key or link kit: owned by somebody who is not
// userID, while ownership is in force. nil or empty means the platform's.
func UserOwnedByOther(flags *services.FeatureFlags, ownerID *string, userID string) bool {
	if ownerID == nil || *ownerID == "" || (userID != "" && *ownerID == userID) {
		return false
	}
	if flags == nil {
		return true
	}
	return flags.BYONOwnershipInForce(context.Background())
}

// KitOwnedByOther is UserOwnedByOther for a route-only link kit, whose owner is
// fenced whenever route-only is on, not only with BYON.
func KitOwnedByOther(flags *services.FeatureFlags, ownerID string, userID string) bool {
	if ownerID == "" || (userID != "" && ownerID == userID) {
		return false
	}
	if flags == nil {
		return true
	}
	return flags.RouteOnlyOwnershipInForce(context.Background())
}

// ForeignToCaller is NodeOwnedByOther for the caller of r. It guards the admin
// and staff WRITES on a machine or on a server by the machine it runs on - move,
// reassign, re-pair, CPU pool, overcommit, demo flag - which are capability-gated
// and so fleet-wide by construction. Callers answer with "not found", like
// diskToolNode, so the refusal does not confirm what exists.
func ForeignToCaller(st store.Store, flags *services.FeatureFlags, r *http.Request, nodeID int) bool {
	return NodeOwnedByOther(st, flags, nodeID, reqctx.ByonCallerID(r))
}

// CanManageNode reports whether the caller may read or configure what is ON a
// node: its servers, its storage and storage placement, its deploy bundle, its
// CPU topology.
//
// An OWNED node answers to its owner only, and being an admin does not change
// that - the same rule CanPlaceOnNode applies, for the same reason. It used to
// short-circuit on admin first, so GET /api/nodes/{id}/servers handed every
// operator the server list of a customer's machine while ListServersForUser
// deliberately withheld the very same rows ("An operator runs the platform; they
// do not run their customers' machines."). Two answers to one question.
//
// The other doors onto a customer's machine ask ForeignToCaller or the resolver
// (SetForeignNode) instead: opening its servers, their files and beam, reset
// pairing and roll key, CPU pool and overcommit, moving, transferring and
// reassigning its servers, the demo flag. Decommissioning (DELETE
// /api/nodes/{id}) goes through none of them, and that one is meant to stay open.
func CanManageNode(flags *services.FeatureFlags, r *http.Request, node *models.Node) bool {
	if node != nil && node.OwnerID != nil && OwnershipInForce(flags, r) {
		return !OwnedByOther(flags, r, node)
	}
	return reqctx.IsAdmin(r)
}

// CanPlaceOnNode reports whether the caller may place a server on a node.
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
func CanPlaceOnNode(flags *services.FeatureFlags, r *http.Request, node *models.Node) bool {
	if node == nil {
		return false
	}
	if node.OwnerID != nil && OwnershipInForce(flags, r) {
		// Somebody's own hardware. Only that somebody, admin or not.
		uid := reqctx.ByonCallerID(r)
		return uid != "" && *node.OwnerID == uid
	}
	// Unowned, or BYON switched off so an owner_id carries no tenancy meaning
	// any more. Operator territory - and it must stay reachable by SOMEONE, or
	// turning the feature off would strand every node that still has an owner.
	return reqctx.IsAdmin(r)
}
