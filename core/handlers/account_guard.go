package handlers

import (
	"net/http"

	"dylaris-core/authz"
	"dylaris-core/models"
)

// mayManageAccount reports whether the caller may change another account's way
// in - its password, 2FA, email, username, role, permission flags, route limit,
// pending deletion - or delete it.
//
// Those routes are gated by users.write (users.delete for the delete), and a
// panel capability is fleet-wide: it says nothing about WHOSE account. So a
// custom panel role holding users.write could reset an admin's password, or
// point an admin's email at its own mailbox and use the reset mail, and log in
// as that admin. The same way reached any staff account holding more than the
// caller, such as panelroles.write.
//
// An admin may manage anyone. Anybody else may manage an account only if it is
// not an admin and holds no PANEL capability the caller lacks. Only panel
// capabilities are compared, plus the legacy per-user rights below: region
// scope and server grants are not.
//
// The target's capabilities are read strictly, not through Resolve, which drops
// a role lookup error and would make a stronger account look powerless. A
// lookup that fails refuses. The caller's side may stay lenient: reading the
// caller as weaker than they are only refuses more.
func mayManageAccount(state *AppState, r *http.Request, target *models.User) bool {
	if IsAdmin(r) {
		return true
	}
	if target == nil || target.IsAdmin || target.Role == "admin" || state == nil || state.Authz == nil || state.Store == nil {
		return false
	}
	held, ok := strictPanelCaps(state, target.ID)
	if !ok {
		return false
	}
	actor, err := state.Authz.Resolve(authz.IdentityFromContext(r.Context()), 0)
	if err != nil {
		return false
	}
	for capID := range held {
		if c, known := authz.Get(capID); known && c.Scope == authz.ScopePanel && !actor.HasCap(capID) {
			return false
		}
	}
	// The per-user rights that are NOT panel caps, which the loop cannot see:
	// role "support" reads and answers every ticket, can_change_resources
	// resizes servers and pins CPUs, and a support team decides whose tickets
	// are visible. Taking such an account over handed the caller a right the
	// permissions route refuses to grant them.
	actorID, _ := r.Context().Value("userID").(string)
	mine := LoadEffectivePermissions(state, actorID)
	if target.Role == "support" && !(mine.IsSupport && mine.CanManageTickets) {
		return false
	}
	if target.CanChangeResources && !mine.CanChangeResources {
		return false
	}
	if target.SupportTeam != "" && target.ID != actorID {
		me, err := state.Store.GetUserByID(actorID)
		if err != nil || me == nil || me.SupportTeam != target.SupportTeam {
			return false
		}
	}
	return true
}

// strictPanelCaps is the panel-role half of Resolve with every error kept.
func strictPanelCaps(state *AppState, userID string) (map[string]bool, bool) {
	roleID, overrides, err := state.Store.GetUserPanelAuthz(userID)
	if err != nil {
		return nil, false
	}
	caps := map[string]bool{}
	if roleID != nil {
		role, rerr := state.Store.GetPanelRole(*roleID)
		if rerr != nil || role == nil {
			return nil, false
		}
		for _, c := range role.Capabilities {
			caps[c] = true
		}
	}
	for _, c := range overrides.Grant {
		caps[c] = true
	}
	for _, c := range overrides.Deny {
		delete(caps, c)
	}
	return caps, true
}

// guardAccountTerms is the rule for what an account is entitled to and
// charged: entitlement grants, limit and billing overrides, billing status.
// mayManageAccount, plus: not the caller's own account unless they are an
// admin. plans.write is not an admin capability, and a holder granting
// themselves BYON, unlimited limits or lifting their own suspension is spending
// what nobody approved. Writes the refusal and returns false.
func guardAccountTerms(w http.ResponseWriter, r *http.Request, state *AppState, userID string) bool {
	if actor, _ := r.Context().Value("userID").(string); !IsAdmin(r) && actor != "" && actor == userID {
		sendJSONError(w, "You cannot change your own account's entitlement, limits or billing", http.StatusForbidden)
		return false
	}
	target, err := state.Store.GetUserByID(userID)
	if err != nil || target == nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return false
	}
	if !mayManageAccount(state, r, target) {
		sendJSONError(w, "You cannot change the terms of an account with more rights than yours", http.StatusForbidden)
		return false
	}
	return true
}

// actingOnSelf reports whether the request is aimed at the caller's own account.
func actingOnSelf(r *http.Request, targetID string) bool {
	actorID, _ := r.Context().Value("userID").(string)
	return actorID != "" && actorID == targetID
}
