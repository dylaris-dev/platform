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
// capabilities are compared: region scope and server grants are not.
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
