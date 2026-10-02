package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"

	"dylaris-core/authz"
	"dylaris-core/store"
)

// ServerRolesHandler serves the level-2 (owner realm) surface: owner-scoped
// server-role CRUD (this file) plus friend assignment (server_grants.go). The
// acting user is always the owner of the roles they manage; the store scopes
// writes by owner id. Gated by AuthMiddleware only for now; phase 4 swaps to
// RequireCap("roles.*") / RequireCap("members.*").
type ServerRolesHandler struct {
	state *AppState
}

func NewServerRolesHandler(state *AppState) *ServerRolesHandler {
	return &ServerRolesHandler{state: state}
}

type serverRoleView struct {
	ID           int      `json:"id"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

func toServerRoleView(sr store.ServerRole) serverRoleView {
	return serverRoleView{ID: sr.ID, Name: sr.Name, Capabilities: normalizeCaps(sr.Capabilities)}
}

// validateServerOwnerCaps returns an error naming the first capability that is
// not a real SERVER- or OWNER-scope capability. The catalog is the single
// source of truth, so an unknown id or a panel-scope id is rejected
// (deny-by-default). An empty slice is valid. Reused by the assign endpoint.
func validateServerOwnerCaps(caps []string) error {
	for _, id := range caps {
		c, ok := authz.Get(id)
		if !ok {
			return errors.New("unknown capability: " + id)
		}
		if c.Scope != authz.ScopeServer && c.Scope != authz.ScopeOwner {
			return errors.New("not a server/owner capability: " + id)
		}
	}
	return nil
}

type serverRoleRequest struct {
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

func actingUserID(r *http.Request) string {
	id, _ := r.Context().Value("userID").(string)
	return id
}

// ListServerRoles GET /api/server-roles - the custom roles in the acting
// owner's realm.
func (h *ServerRolesHandler) ListServerRoles(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	owner := actingUserID(r)
	if owner == "" {
		sendJSONError(w, "Forbidden", 403)
		return
	}
	roles, err := h.state.Store.ListServerRolesByOwner(owner)
	if err != nil {
		sendJSONError(w, "Failed to list server roles", 500)
		return
	}
	views := make([]serverRoleView, 0, len(roles))
	for _, sr := range roles {
		views = append(views, toServerRoleView(sr))
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "roles": views})
}

// CreateServerRole POST /api/server-roles - adds a custom role to the acting
// owner's realm. Outside advanced permissions mode a non-admin is refused with
// 403, and a duplicate name is 409.
func (h *ServerRolesHandler) CreateServerRole(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	idn := authz.IdentityFromContext(r.Context())
	if !idn.IsAdmin && PermissionsMode(h.state.Store) != authz.ModeAdvanced {
		sendJSONError(w, "Custom roles require advanced permissions mode", 403)
		return
	}
	owner := actingUserID(r)
	if owner == "" {
		sendJSONError(w, "Forbidden", 403)
		return
	}
	var req serverRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	if req.Name == "" {
		sendJSONError(w, "Name is required", 400)
		return
	}
	if err := validateServerOwnerCaps(req.Capabilities); err != nil {
		sendJSONError(w, err.Error(), 400)
		return
	}
	id, err := h.state.Store.CreateServerRole(owner, req.Name, req.Capabilities)
	if err != nil {
		sendJSONError(w, "Failed to create server role (the name may already be in use)", 409)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"role":    serverRoleView{ID: id, Name: req.Name, Capabilities: normalizeCaps(req.Capabilities)},
	})
}

// UpdateServerRole PATCH /api/server-roles/{id} - renames a role and replaces
// its capabilities. Scoped to the acting owner, so another realm's role is
// 404.
func (h *ServerRolesHandler) UpdateServerRole(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	idn := authz.IdentityFromContext(r.Context())
	if !idn.IsAdmin && PermissionsMode(h.state.Store) != authz.ModeAdvanced {
		sendJSONError(w, "Custom roles require advanced permissions mode", 403)
		return
	}
	owner := actingUserID(r)
	if owner == "" {
		sendJSONError(w, "Forbidden", 403)
		return
	}
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		sendJSONError(w, "Invalid role ID", 400)
		return
	}
	var req serverRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	if req.Name == "" {
		sendJSONError(w, "Name is required", 400)
		return
	}
	if err := validateServerOwnerCaps(req.Capabilities); err != nil {
		sendJSONError(w, err.Error(), 400)
		return
	}
	if err := h.state.Store.UpdateServerRole(id, owner, req.Name, req.Capabilities); err != nil {
		sendJSONError(w, "Server role not found", 404)
		return
	}
	// A role's capabilities are every holder's: changing them changes who may
	// reach the files of every server in this realm.
	stampBeamAccessForOwner(r.Context(), h.state, owner)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"role":    serverRoleView{ID: id, Name: req.Name, Capabilities: normalizeCaps(req.Capabilities)},
	})
}

// DeleteServerRole DELETE /api/server-roles/{id} - deletes a role in the
// acting owner's realm; another realm's role is 404.
func (h *ServerRolesHandler) DeleteServerRole(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	idn := authz.IdentityFromContext(r.Context())
	if !idn.IsAdmin && PermissionsMode(h.state.Store) != authz.ModeAdvanced {
		sendJSONError(w, "Custom roles require advanced permissions mode", 403)
		return
	}
	owner := actingUserID(r)
	if owner == "" {
		sendJSONError(w, "Forbidden", 403)
		return
	}
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		sendJSONError(w, "Invalid role ID", 400)
		return
	}
	if err := h.state.Store.DeleteServerRole(id, owner); err != nil {
		sendJSONError(w, "Server role not found", 404)
		return
	}
	stampBeamAccessForOwner(r.Context(), h.state, owner)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
