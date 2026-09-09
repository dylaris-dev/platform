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

// PanelRolesHandler serves the level-1 (staff) panel-role admin surface. Every
// endpoint is gated at the route with
// RequireCap("panelroles.read"/"panelroles.write"/"panelroles.delete");
// admin short-circuits every PANEL cap.
type PanelRolesHandler struct {
	state *AppState
}

func NewPanelRolesHandler(state *AppState) *PanelRolesHandler {
	return &PanelRolesHandler{state: state}
}

type panelRoleView struct {
	ID           int      `json:"id"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
	IsSystem     bool     `json:"isSystem"`
}

func normalizeCaps(caps []string) []string {
	if caps == nil {
		return []string{}
	}
	return caps
}

func toPanelRoleView(pr store.PanelRole) panelRoleView {
	return panelRoleView{ID: pr.ID, Name: pr.Name, Capabilities: normalizeCaps(pr.Capabilities), IsSystem: pr.IsSystem}
}

// validatePanelCaps returns an error naming the first capability that is not a
// real PANEL-scope capability. The catalog is the single source of truth, so an
// unknown id or a server/owner-scope id is rejected (deny-by-default). An empty
// slice is valid. Reused by the per-user assignment endpoint (user_role_perms.go).
func validatePanelCaps(caps []string) error {
	for _, id := range caps {
		c, ok := authz.Get(id)
		if !ok {
			return errors.New("unknown capability: " + id)
		}
		if c.Scope != authz.ScopePanel {
			return errors.New("not a panel capability: " + id)
		}
	}
	return nil
}

func parsePanelRoleID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		sendJSONError(w, "Invalid role ID", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

type panelRoleRequest struct {
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

// ListPanelRoles GET /api/admin/panel-roles - the level-1 staff roles and
// their capabilities.
func (h *PanelRolesHandler) ListPanelRoles(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	roles, err := h.state.Store.ListPanelRoles()
	if err != nil {
		sendJSONError(w, "Failed to list panel roles", 500)
		return
	}
	views := make([]panelRoleView, 0, len(roles))
	for _, pr := range roles {
		views = append(views, toPanelRoleView(pr))
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "roles": views})
}

type panelAssignmentView struct {
	UserID      string   `json:"userId"`
	PanelRoleID *int     `json:"panelRoleId"`
	GrantCaps   []string `json:"grantCaps"`
	DenyCaps    []string `json:"denyCaps"`
}

// ListPanelAssignments GET /api/admin/panel-roles/assignments - who holds a
// panel role or a per-user override, all of them at once.
//
// It exists because the only way to read this was one user at a time, so the
// Roles screen could show a person's panel role only after somebody clicked
// them. It listed the LEGACY role in that column instead, which is a different
// concept - the button beside it edited the panel role, and the two disagreed
// on every staff member.
//
// Under panelroles.read rather than hung off /users deliberately. /users runs
// under users.read, and who has which privileges is a different question from
// who has an account: a reader allowed to list people should not learn the
// privilege map for free. It returns ids only, so it cannot be used to
// enumerate accounts either.
func (h *PanelRolesHandler) ListPanelAssignments(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	assignments, err := h.state.Store.ListUserPanelAssignments()
	if err != nil {
		sendJSONError(w, "Failed to list panel role assignments", 500)
		return
	}
	views := make([]panelAssignmentView, 0, len(assignments))
	for _, a := range assignments {
		views = append(views, panelAssignmentView{
			UserID:      a.UserID,
			PanelRoleID: a.PanelRoleID,
			GrantCaps:   normalizeCaps(a.CapOverrides.Grant),
			DenyCaps:    normalizeCaps(a.CapOverrides.Deny),
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "assignments": views})
}

// CreatePanelRole POST /api/admin/panel-roles - adds a staff role. A duplicate
// name answers 409 with that reason rather than a bare 500.
func (h *PanelRolesHandler) CreatePanelRole(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	var req panelRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	if req.Name == "" {
		sendJSONError(w, "Name is required", 400)
		return
	}
	if err := validatePanelCaps(req.Capabilities); err != nil {
		sendJSONError(w, err.Error(), 400)
		return
	}
	actorID, _ := r.Context().Value("userID").(string)
	var createdBy *string
	if actorID != "" {
		createdBy = &actorID
	}
	id, err := h.state.Store.CreatePanelRole(req.Name, req.Capabilities, createdBy)
	if err != nil {
		// The dominant failure is the UNIQUE(name) violation; surface it as a
		// conflict with a clear message rather than a bare 500.
		sendJSONError(w, "Failed to create panel role (the name may already be in use)", 409)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"role":    panelRoleView{ID: id, Name: req.Name, Capabilities: normalizeCaps(req.Capabilities), IsSystem: false},
	})
}

// UpdatePanelRole PATCH /api/admin/panel-roles/{id} - renames a staff role and
// replaces its capability set.
func (h *PanelRolesHandler) UpdatePanelRole(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	id, ok := parsePanelRoleID(w, r)
	if !ok {
		return
	}
	var req panelRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	if req.Name == "" {
		sendJSONError(w, "Name is required", 400)
		return
	}
	if err := validatePanelCaps(req.Capabilities); err != nil {
		sendJSONError(w, err.Error(), 400)
		return
	}
	existing, err := h.state.Store.GetPanelRole(id)
	if err != nil || existing == nil {
		sendJSONError(w, "Panel role not found", 404)
		return
	}
	if existing.IsSystem {
		sendJSONError(w, "System roles cannot be edited", 403)
		return
	}
	if err := h.state.Store.UpdatePanelRole(id, req.Name, req.Capabilities); err != nil {
		sendJSONError(w, "Failed to update panel role", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"role":    panelRoleView{ID: id, Name: req.Name, Capabilities: normalizeCaps(req.Capabilities), IsSystem: false},
	})
}

// DeletePanelRole DELETE /api/admin/panel-roles/{id} - deletes a staff role.
// System roles are refused with 403; an unknown id is 404.
func (h *PanelRolesHandler) DeletePanelRole(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	id, ok := parsePanelRoleID(w, r)
	if !ok {
		return
	}
	existing, err := h.state.Store.GetPanelRole(id)
	if err != nil || existing == nil {
		sendJSONError(w, "Panel role not found", 404)
		return
	}
	if existing.IsSystem {
		sendJSONError(w, "System roles cannot be deleted", 403)
		return
	}
	if err := h.state.Store.DeletePanelRole(id); err != nil {
		sendJSONError(w, "Failed to delete panel role", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
