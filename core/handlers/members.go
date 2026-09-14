package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type MemberHandler struct {
	state *AppState
}

func NewMemberHandler(state *AppState) *MemberHandler {
	return &MemberHandler{state: state}
}

// capPermissions caps a requested permission set to what the CALLER may
// delegate onward. Owner and admin are uncapped; everyone else may grant a key
// only if they themselves hold every capability that key confers.
//
// It used to read the server_invites row for (serverID, callerID) and cap
// against that row's legacy booleans. Two things went wrong with that:
//
//   - An ACCOUNT-WIDE grantee has no such row by construction (their grant is
//     the row with server_id IS NULL), so the lookup returned sql.ErrNoRows and
//     the function returned the requested permissions UNCAPPED. They passed
//     RequireCap("members.write") because the resolver folds account-wide
//     grants in - and then delegated the full set on any server in that owner's
//     realm, whatever their own grant allowed. CreateInvite runs the map
//     through store.MapLegacyInviteCaps, so those keys become real capability
//     grants, not a display field.
//   - Every error path returned the permissions uncapped as well, so a database
//     blip widened a delegation instead of refusing it.
//
// Asking the resolver fixes both at once and covers every grant source there
// is - per-server row, proxy-inherited, account-wide, server role - rather than
// the one table this happened to read. Same lesson as the SFTP grant lookup:
// the resolver is the authority, a grant table is not.
func (h *MemberHandler) capPermissions(r *http.Request, serverID int, perms map[string]bool) map[string]bool {
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	userID, _ := r.Context().Value("userID").(string)
	username, _ := r.Context().Value("username").(string)

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil || srv == nil {
		return denyAllPermissions(perms)
	}
	// Owner short-circuit keyed on the immutable owner UUID, never the mutable
	// username (a rename frees the old name, which a squatter could reclaim),
	// matching the resolver's rule (see authz/resolver.go). The route itself is
	// already gated by RequireCap("members.write"); this only caps HOW MUCH an
	// invited non-owner may delegate onward.
	// An admin on a customer's machine got here through that customer's invite,
	// so the invite, not the admin flag, caps what they may hand on.
	if srv.OwnerID == userID || (isAdmin && !h.state.NodeOwnedByOther(srv.NodeID, userID)) {
		return perms
	}
	if h.state.Authz == nil {
		return denyAllPermissions(perms)
	}
	res, rerr := h.state.Authz.Resolve(authz.Identity{UserID: userID, Username: username, IsAdmin: isAdmin}, serverID)
	if rerr != nil {
		return denyAllPermissions(perms)
	}

	capped := make(map[string]bool, len(perms))
	for k, v := range perms {
		capped[k] = v && callerMayDelegate(res, k)
	}
	return capped
}

// denyAllPermissions keeps every requested key present but false. Dropping the
// keys instead would let a caller's map decide what CreateInvite writes; an
// explicit false is the same shape with none of the permission.
func denyAllPermissions(perms map[string]bool) map[string]bool {
	out := make(map[string]bool, len(perms))
	for k := range perms {
		out[k] = false
	}
	return out
}

// callerMayDelegate reports whether the caller holds everything one legacy
// invite key confers.
//
// The key's meaning comes from store.MapLegacyInviteCaps - the very function
// CreateInvite uses to turn these booleans into capability grants - so the two
// sides cannot drift apart about what "files" or "power" means.
//
// "inherit" is capped to false for anyone but the owner and an admin. It is not
// in that mapping because it confers no capability of its own: it makes a grant
// flow down to child servers, which widens the blast radius across the server
// TREE rather than within one server. That is the owner's call about their own
// topology, not something a delegated inviter should hand on. (The previous
// code let a member with inherit pass it along; this narrows that.)
func callerMayDelegate(res *authz.Resolution, key string) bool {
	if key == "inherit" {
		return false
	}
	var tp models.TabPermissions
	switch key {
	case "console":
		tp.Console = true
	case "files":
		tp.Files = true
	case "config":
		tp.Config = true
	case "setup":
		tp.Setup = true
	case "power":
		tp.Power = true
	case "members":
		tp.Members = true
	case "overview":
		tp.Overview = true
	case "network":
		tp.Network = true
	default:
		// An unrecognised key confers nothing: MapLegacyInviteCaps ignores it,
		// so it cannot grant capabilities however it is stored.
		return true
	}
	for _, c := range store.MapLegacyInviteCaps(tp) {
		if !res.HasCap(c) {
			return false
		}
	}
	return true
}

// GetMembers GET /api/servers/{id}/members - the member invites on one server.
func (h *MemberHandler) GetMembers(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", http.StatusBadRequest)
		return
	}

	invites, err := h.state.Store.ListInvitesByServer(serverID)
	if err != nil {
		sendJSONError(w, "Failed to load members", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"members": invites,
	})
}

// InviteMember POST /api/servers/{id}/members - invites a user onto the server
// with a permission set. Server auditing is switched on first if it was off,
// so the invite itself is the first thing the audit trail records. An existing
// member is 409.
func (h *MemberHandler) InviteMember(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Username    string          `json:"username"`
		Permissions map[string]bool `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Username == "" {
		sendJSONError(w, "Username required", http.StatusBadRequest)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}

	targetUser, err := h.state.Store.GetUserByUsername(req.Username)
	if err != nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}

	if targetUser.Username == srv.OwnerName {
		sendJSONError(w, "Cannot invite the server owner", http.StatusBadRequest)
		return
	}

	inviterID := ""
	if id, ok := r.Context().Value("userID").(string); ok {
		inviterID = id
	}

	// Default shape grants everything except member management.
	if req.Permissions == nil {
		req.Permissions = map[string]bool{
			"console": true, "files": true,
			"config": true, "setup": true, "overview": true,
			"power": true, "members": false,
		}
	}

	// Cap to what the inviter themselves holds.
	req.Permissions = h.capPermissions(r, serverID, req.Permissions)

	if err := h.state.Store.CreateInvite(serverID, targetUser.ID, inviterID, req.Permissions); err != nil {
		sendJSONError(w, "Failed to create invite (user may already be invited)", http.StatusConflict)
		return
	}

	// First member invite flips audit_enabled on. Cheap no-op when
	// already on. The audit row for the invite is written right after.
	EnableServerAuditIfNeeded(h.state, serverID)
	LogServerAudit(h.state, r, serverID, ServerAuditEventMemberInvited, inviterID, targetUser.ID, map[string]interface{}{
		"username":    targetUser.Username,
		"permissions": req.Permissions,
	})

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// UpdateMemberPermissions PATCH /api/servers/{id}/members/{userId} - replaces
// one member's permission set and records the change in the server audit
// trail.
func (h *MemberHandler) UpdateMemberPermissions(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", http.StatusBadRequest)
		return
	}
	targetUserID, ok := parseUserID(w, r, "userId")
	if !ok {
		return
	}

	var req struct {
		Permissions map[string]bool `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Cap to what the inviter themselves holds.
	req.Permissions = h.capPermissions(r, serverID, req.Permissions)

	if err := h.state.Store.UpdateInvitePermissions(serverID, targetUserID, req.Permissions); err != nil {
		sendJSONError(w, "Failed to update permissions", http.StatusInternalServerError)
		return
	}

	actorID, _ := r.Context().Value("userID").(string)
	LogServerAudit(h.state, r, serverID, ServerAuditEventMemberPermsChanged, actorID, targetUserID, map[string]interface{}{
		"permissions": req.Permissions,
	})

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// RemoveMember DELETE /api/servers/{id}/members/{userId} - revokes a member's
// access and records it in the server audit trail.
func (h *MemberHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", http.StatusBadRequest)
		return
	}
	targetUserID, ok := parseUserID(w, r, "userId")
	if !ok {
		return
	}

	if err := h.state.Store.DeleteInvite(serverID, targetUserID); err != nil {
		sendJSONError(w, "Failed to remove member", http.StatusInternalServerError)
		return
	}

	actorID, _ := r.Context().Value("userID").(string)
	LogServerAudit(h.state, r, serverID, ServerAuditEventMemberRemoved, actorID, targetUserID, nil)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// GetInheritedMembers GET /api/servers/{id}/members/inherited
// Returns users who have access via proxy inheritance (read-only view).
func (h *MemberHandler) GetInheritedMembers(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", http.StatusBadRequest)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}

	// Only child servers (those with a proxy_id) can have inherited members
	if srv.ProxyID == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"members": []interface{}{},
		})
		return
	}

	// Get all invites on the proxy that have inherit=true
	proxyInvites, err := h.state.Store.ListInvitesByServer(*srv.ProxyID)
	if err != nil {
		sendJSONError(w, "Failed to load inherited members", http.StatusInternalServerError)
		return
	}

	var inherited []interface{}
	for _, inv := range proxyInvites {
		if inv.Permissions.Inherit {
			// Exclude the server owner (they already have full access)
			if inv.UserID == srv.OwnerID {
				continue
			}
			// Exclude users who have a direct invite on this server
			_, directErr := h.state.Store.GetInvite(serverID, inv.UserID)
			if directErr == nil {
				continue
			}
			inherited = append(inherited, map[string]interface{}{
				"userId":      inv.UserID,
				"username":    inv.Username,
				"permissions": inv.Permissions,
				"proxyId":     *srv.ProxyID,
			})
		}
	}

	if inherited == nil {
		inherited = []interface{}{}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"members": inherited,
	})
}
