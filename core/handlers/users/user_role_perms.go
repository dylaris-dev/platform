package users

import (
	"encoding/json"
	"net/http"

	"dylaris-core/handlers"

	"dylaris-core/store"
)

// These endpoints live on UserHandler alongside the existing user CRUD —
// kept in a separate file so the additions don't bloat users.go.

type setRoleRequest struct {
	Role string `json:"role"`
	handlers.AdminReauthRequest
}

// SetUserRole PUT /api/admin/users/{id}/role
// Valid roles: "user", "support", "admin". Admins cannot demote themselves
// — prevents the last-admin-locks-themselves-out scenario.
func (h *UserHandler) SetUserRoleHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		handlers.SendJSONError(w, "Database not connected", 503)
		return
	}
	id, ok := handlers.ParseUserID(w, r)
	if !ok {
		return
	}
	var req setRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", 400)
		return
	}
	if req.Role != "user" && req.Role != "support" && req.Role != "admin" {
		handlers.SendJSONError(w, "Invalid role (must be user, support, or admin)", 400)
		return
	}
	target, err := h.state.Store.GetUserByID(id)
	if err != nil || target == nil {
		handlers.SendJSONError(w, "User not found", 404)
		return
	}
	previousRole := target.Role
	if previousRole == "" {
		previousRole = "user"
	}
	// users.write is delegatable; promoting someone - yourself included - is not
	// something a delegated right can do. "support" too: the boot backfill turns
	// role 'support' into the seeded support PANEL role for any account without
	// one, which hands a sock-puppet rights the caller never held. The role it
	// already has is not a promotion: the panel re-sends it on every save.
	if req.Role != "user" && req.Role != previousRole && !handlers.IsAdmin(r) {
		handlers.SendJSONError(w, "Only an admin can give someone the admin or support role", http.StatusForbidden)
		return
	}

	// Self-demotion guard: an admin demoting themselves is fine as long as
	// at least one other admin remains. Otherwise refuse — the operator
	// would lock themselves out of /admin/* immediately.
	actorID, _ := r.Context().Value("userID").(string)
	if actorID == id && req.Role != "admin" {
		users, _ := h.state.Store.ListUsers()
		adminCount := 0
		for _, u := range users {
			if u.IsAdmin && u.ID != id {
				adminCount++
			}
		}
		if adminCount == 0 {
			handlers.SendJSONError(w, "Cannot demote the last admin — promote someone else first", 409)
			return
		}
	}

	if !handlers.MayManageAccount(h.state, r, target) {
		handlers.SendJSONError(w, "You cannot change the role of an account with more rights than yours", http.StatusForbidden)
		return
	}

	// A change of role hands out (or takes away) durable power, so the
	// administrator proves who they are. Last, after every cheap check that can
	// refuse on its own - a caller who is about to be told "last admin" or
	// "more rights than yours" should hear that, not a password prompt.
	//
	// Only a REAL change asks: the panel re-sends the current role on every
	// save, which is the same reason the promotion guard above compares against
	// previousRole.
	if req.Role != previousRole && !handlers.RequireAdminReauth(w, r, h.state, req.AdminReauthRequest) {
		return
	}

	// What the account reaches under its OLD role - every server, for an
	// administrator - is listed now and stamped once the change is committed,
	// so a ticket minted as the old role does not outlive it.
	var reach []string
	if req.Role != previousRole {
		reach = handlers.BeamReachOf(h.state, id)
	}
	if err := h.state.Store.SetUserRole(id, req.Role); err != nil {
		handlers.SendJSONError(w, "Failed to update role", 500)
		return
	}
	handlers.StampBeamUUIDs(r.Context(), h.state, reach)

	handlers.LogIdentityAudit(h.state, r, handlers.AuditEventUserRoleChanged, actorID, id, map[string]interface{}{
		"from": previousRole,
		"to":   req.Role,
	})

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"role":    req.Role,
	})
}

type setPermissionsRequest struct {
	CanDeleteServers   bool   `json:"canDeleteServers"`
	CanChangeResources bool   `json:"canChangeResources"`
	SupportTeam        string `json:"supportTeam"`
	handlers.AdminReauthRequest
}

// SetUserPermissions PUT /api/admin/users/{id}/permissions
// Sets per-user capability flags. Admin overrides still apply at the
// EffectivePermissions level — these flags only ever matter for non-admins.
func (h *UserHandler) SetUserPermissionsHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		handlers.SendJSONError(w, "Database not connected", 503)
		return
	}
	id, ok := handlers.ParseUserID(w, r)
	if !ok {
		return
	}
	var req setPermissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", 400)
		return
	}

	// Deleting servers follows the ROLE, so it is never stored true for a
	// non-admin. Forced here rather than rejected: the screen no longer offers
	// the switch, so a request carrying it is a stale client or a direct API
	// call, and neither is a reason to fail an otherwise valid save.
	//
	// Without this the row would keep claiming a right that
	// ComputeEffectivePermissions no longer grants - a stored value nothing
	// reads, which is how a permissions screen starts lying.
	// A lookup that fails forces it false rather than refusing the save. The
	// conservative direction is the one that grants nothing, and an operator
	// editing the OTHER flags must not be blocked because one read hiccuped -
	// this endpoint used to need no read at all.
	target, terr := h.state.Store.GetUserByID(id)
	if terr != nil || target == nil || !(target.IsAdmin || target.Role == "admin") {
		req.CanDeleteServers = false
	}
	if !handlers.IsAdmin(r) && (terr != nil || target == nil || !handlers.MayManageAccount(h.state, r, target)) {
		handlers.SendJSONError(w, "You cannot change the permissions of an account with more rights than yours", http.StatusForbidden)
		return
	}
	// mayManageAccount passes on yourself, and the support team decides which
	// tickets a staff member sees: a staff member could move themselves into
	// any team, or through all of them, past the cross-team visibility setting.
	if !handlers.IsAdmin(r) && handlers.ActingOnSelf(r, id) {
		handlers.SendJSONError(w, "Your own permissions and support team are set by another administrator", http.StatusForbidden)
		return
	}
	// mayManageAccount compares the team the target is in, not the one asked
	// for: staff could take over a colleague, move them into another team and
	// read that team's tickets as them. Staff place someone only in their own.
	if !handlers.IsAdmin(r) && req.SupportTeam != "" && req.SupportTeam != target.SupportTeam {
		actorID, _ := r.Context().Value("userID").(string)
		me, err := h.state.Store.GetUserByID(actorID)
		if err != nil || me == nil || me.SupportTeam != req.SupportTeam {
			handlers.SendJSONError(w, "You can only place someone in your own support team", http.StatusForbidden)
			return
		}
	}
	// Resource changes are a right of their own; a delegated users.write must
	// not hand it out, to someone else or to the caller themselves. Only a
	// change from off to on hands it out: the panel re-sends the current value.
	if req.CanChangeResources && !handlers.IsAdmin(r) && !target.CanChangeResources {
		actorID, _ := r.Context().Value("userID").(string)
		if !handlers.LoadEffectivePermissions(h.state, actorID).CanChangeResources {
			handlers.SendJSONError(w, "You cannot grant resource changes you do not hold", http.StatusForbidden)
			return
		}
	}

	// Only a real change asks for the password: the panel re-sends the current
	// values on every save, and a save that grants nothing is not the thing
	// this guard is for. A target that could not be read counts as changed -
	// the conservative direction, since the alternative is writing flags
	// without proving who asked.
	changed := target == nil ||
		target.CanDeleteServers != req.CanDeleteServers ||
		target.CanChangeResources != req.CanChangeResources ||
		target.SupportTeam != req.SupportTeam
	if changed && !handlers.RequireAdminReauth(w, r, h.state, req.AdminReauthRequest) {
		return
	}

	if err := h.state.Store.SetUserPermissionFlags(id, req.CanDeleteServers, req.CanChangeResources, req.SupportTeam); err != nil {
		handlers.SendJSONError(w, "Failed to update permissions", 500)
		return
	}

	actorID, _ := r.Context().Value("userID").(string)
	handlers.LogIdentityAudit(h.state, r, handlers.AuditEventUserPermissionsChanged, actorID, id, map[string]interface{}{
		"can_delete_servers":   req.CanDeleteServers,
		"can_change_resources": req.CanChangeResources,
		"support_team":         req.SupportTeam,
	})

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":            true,
		"canDeleteServers":   req.CanDeleteServers,
		"canChangeResources": req.CanChangeResources,
		"supportTeam":        req.SupportTeam,
	})
}

type setPanelRoleRequest struct {
	PanelRoleID *int     `json:"panelRoleId"`
	GrantCaps   []string `json:"grantCaps"`
	DenyCaps    []string `json:"denyCaps"`
	handlers.AdminReauthRequest
}

// SetUserPanelRoleHandler PUT /api/admin/users/{id}/panel-role
// Assigns (panelRoleId) or clears (panelRoleId=null) the user's level-1 panel
// role and their per-user panel cap grant/deny overrides. Gated at the route
// with RequireCap("panelroles.write"); admin short-circuits. Every override
// cap must be a real PANEL-scope capability, and a non-null panelRoleId must
// reference an existing role. The legacy role/is_admin path is untouched.
func (h *UserHandler) SetUserPanelRoleHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		handlers.SendJSONError(w, "Database not connected", 503)
		return
	}
	id, ok := handlers.ParseUserID(w, r)
	if !ok {
		return
	}
	var req setPanelRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", 400)
		return
	}
	// Override caps must be real PANEL-scope caps (validatePanelCaps lives in
	// panel_roles.go, same package).
	if err := handlers.ValidatePanelCaps(req.GrantCaps); err != nil {
		handlers.SendJSONError(w, err.Error(), 400)
		return
	}
	if err := handlers.ValidatePanelCaps(req.DenyCaps); err != nil {
		handlers.SendJSONError(w, err.Error(), 400)
		return
	}
	if req.PanelRoleID != nil {
		role, err := h.state.Store.GetPanelRole(*req.PanelRoleID)
		if err != nil || role == nil {
			handlers.SendJSONError(w, "Panel role not found", 404)
			return
		}
	}
	// A panel role is the level-1 grant - panelroles.write alone is effectively
	// full admin - so this asks every time rather than comparing against the
	// current assignment. Unlike the role and flags above, nothing here is
	// re-sent by an unrelated save: this endpoint is called to CHANGE the
	// assignment, and the read it would take to tell a no-op apart is not worth
	// a branch on the one action that hands out panel power.
	if !handlers.RequireAdminReauth(w, r, h.state, req.AdminReauthRequest) {
		return
	}

	if err := h.state.Store.SetUserPanelRole(id, req.PanelRoleID); err != nil {
		handlers.SendJSONError(w, "Failed to set panel role", 500)
		return
	}
	ov := store.CapOverrides{Grant: req.GrantCaps, Deny: req.DenyCaps}
	if err := h.state.Store.SetUserPanelCapOverrides(id, ov); err != nil {
		handlers.SendJSONError(w, "Failed to set overrides", 500)
		return
	}

	actorID, _ := r.Context().Value("userID").(string)
	handlers.LogIdentityAudit(h.state, r, handlers.AuditEventUserPanelRoleChanged, actorID, id, map[string]interface{}{
		"panel_role_id": req.PanelRoleID,
		"grant_caps":    req.GrantCaps,
		"deny_caps":     req.DenyCaps,
	})

	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// GetUserPanelRoleHandler GET /api/admin/users/{id}/panel-role
// Reads the user's level-1 panel role id + per-user override caps, symmetric
// with the PUT. Gated at the route with RequireCap("panelroles.read"); admin
// short-circuits. Lets the RolesTab pre-fill the assignment editor.
func (h *UserHandler) GetUserPanelRoleHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		handlers.SendJSONError(w, "Database not connected", 503)
		return
	}
	id, ok := handlers.ParseUserID(w, r)
	if !ok {
		return
	}
	roleID, ov, err := h.state.Store.GetUserPanelAuthz(id)
	if err != nil {
		handlers.SendJSONError(w, "User not found", 404)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"panelRoleId": roleID,
		"grantCaps":   handlers.CatalogCaps(ov.Grant),
		"denyCaps":    handlers.CatalogCaps(ov.Deny),
	})
}
