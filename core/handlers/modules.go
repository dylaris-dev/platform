package handlers

import (
	"dylaris-core/models"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
)

type ModuleHandler struct {
	state *AppState
}

func NewModuleHandler(state *AppState) *ModuleHandler {
	return &ModuleHandler{state: state}
}

// GetModulesHandler GET /api/modules - the navbar module rows. Seeds the four
// defaults when the table is empty, and filters out the legacy Gateway row,
// whose UI moved into the Infrastructure module's Routes tab.
func (h *ModuleHandler) GetModulesHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	modules, err := h.state.Store.ListModules()
	if err != nil {
		sendJSONError(w, "Failed to load modules", 500)
		return
	}

	// === AUTO-SEED LOGIK ===
	if len(modules) == 0 {
		defaultModules := []models.Module{
			{Name: "Servers", Type: "internal", Icon: "server", URL: "/servers", IsEnabled: true, IsSystem: true, Position: 1, AccessRole: "all"},
			{Name: "Admin", Type: "internal", Icon: "shield-check", URL: "/admin", IsEnabled: true, IsSystem: true, Position: 2, AccessRole: "admin"},
			{Name: "Infrastructure", Type: "internal", Icon: "cpu", URL: "/infrastructure", IsEnabled: true, IsSystem: true, Position: 3, AccessRole: "admin"},
			{Name: "Library", Type: "internal", Icon: "folder-open", URL: "/library", IsEnabled: false, IsSystem: false, Position: 4, AccessRole: "admin"},
		}

		for _, m := range defaultModules {
			mod := m // Pointer copy
			h.state.Store.CreateModule(&mod)
		}

		// Reload list after insert
		modules, _ = h.state.Store.ListModules()
	}

	// Gateway is no longer a standalone module — its UI was merged into the
	// Infrastructure module's Routes tab. Filter any legacy "Gateway" row out
	// of the response so the panel never shows the old nav entry.
	if len(modules) > 0 {
		filtered := modules[:0]
		for _, m := range modules {
			if m.Name == "Gateway" {
				continue
			}
			filtered = append(filtered, m)
		}
		modules = filtered
	}

	if modules == nil {
		modules = []models.Module{}
	}

	// Non-admins only see modules with access_role = "all". Admins see all
	// modules (including the admin-only ones) and the access_role field so
	// they can configure visibility from the Modules tab.
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	if !isAdmin {
		filtered := modules[:0]
		for _, m := range modules {
			if m.AccessRole != "admin" {
				filtered = append(filtered, m)
			}
		}
		modules = filtered
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"modules": modules,
	})
}

// SetModuleAccessRoleHandler PATCH /modules/{id}/role - RequireCap("settings.write")
// at the route (Phase 4 Task 19). Body: {"role": "all" | "admin"}. Servers stays
// "all" regardless.
func (h *ModuleHandler) SetModuleAccessRoleHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	if req.Role != "all" && req.Role != "admin" {
		sendJSONError(w, "role must be 'all' or 'admin'", 400)
		return
	}
	mod, err := h.state.Store.GetModuleByID(id)
	if err != nil {
		sendJSONError(w, "Module not found", 404)
		return
	}
	if mod.Name == "Servers" {
		sendJSONError(w, "Servers module is always visible to all users", 400)
		return
	}
	if mod.Name == "Admin" {
		sendJSONError(w, "Admin module is always admin-only", 400)
		return
	}
	if mod.Name == "Infrastructure" {
		sendJSONError(w, "Infrastructure module is always admin-only", 400)
		return
	}
	if err := h.state.Store.SetModuleAccessRole(id, req.Role); err != nil {
		sendJSONError(w, "Update failed", 500)
		return
	}
	h.state.Events.Publish(r.Context(), "modules.changed", nil)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// CreateModuleHandler POST /modules - RequireCap("settings.write") at the route
// (Phase 4 Task 19).
func (h *ModuleHandler) CreateModuleHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	var req models.Module
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	if req.IsSystem {
		sendJSONError(w, "System modules cannot be created via API", 403)
		return
	}
	// An iframe module is shown to every user it is visible to. Its address
	// was taken as sent - only the form said "url" - so the same rule as a
	// custom tab's applies: a parsable http or https address.
	if req.Type == "iframe" || req.URL != "" {
		if err := validateTabURL(req.URL); err != nil {
			sendJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	id, err := h.state.Store.CreateModule(&req)
	if err != nil {
		sendJSONError(w, "Create failed", 500)
		return
	}

	req.ID = id
	req.IsEnabled = true
	h.state.Events.Publish(r.Context(), "modules.changed", nil)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "module": req})
}

// builtInModules cannot be deleted (only toggled if !is_system).
// They're seeded by Core and re-created on every start anyway, so deleting
// them just causes confusion + the next restart re-creates them.
// Note: "Gateway" is intentionally absent — it was retired as a standalone
// module and its content moved into Infrastructure's Routes tab.
// Note: "Tickets" is deliberately is_system=false (see database/db_tables.go
// seedSystemModules) so it stays toggle-able (enable/disable) via the
// !is_system path in ToggleModuleHandler; its delete protection therefore
// has to come from this name-based guard, same as Library, rather than from
// flipping is_system=true (which would also block disabling it).
var builtInModules = map[string]bool{
	"Servers":        true,
	"Admin":          true,
	"Infrastructure": true,
	"Library":        true,
	"Tickets":        true,
	// Seeded like Tickets and equally is_system=false so it stays toggle-able.
	// Deleting it strands whatever tabs already exist and the next restart
	// re-creates the row anyway.
	"Custom Tabs": true,
}

// DeleteModuleHandler DELETE /modules/{id} - RequireCap("settings.write") at the
// route (Phase 4 Task 19).
func (h *ModuleHandler) DeleteModuleHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	mod, err := h.state.Store.GetModuleByID(id)
	if err != nil || mod == nil {
		sendJSONError(w, "Module not found", 404)
		return
	}
	if mod.IsSystem || builtInModules[mod.Name] {
		sendJSONError(w, "Built-in modules cannot be deleted", 400)
		return
	}

	if err := h.state.Store.DeleteModule(id); err != nil {
		sendJSONError(w, "Delete failed", 500)
		return
	}
	h.state.Events.Publish(r.Context(), "modules.changed", nil)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// UpdateModulePositionHandler PATCH /modules/{id}/position - RequireCap(
// "settings.write") at the route (Phase 4 Task 19).
func (h *ModuleHandler) UpdateModulePositionHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	var req struct {
		Position int `json:"position"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	if err := h.state.Store.UpdateModulePosition(id, req.Position); err != nil {
		sendJSONError(w, "Update failed", 500)
		return
	}
	h.state.Events.Publish(r.Context(), "modules.changed", nil)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ToggleModuleHandler for enabling/disabling modules - RequireCap("settings.write")
// at the route (Phase 4 Task 19).
func (h *ModuleHandler) ToggleModuleHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	var req struct {
		IsEnabled bool `json:"isEnabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	// Prevent disabling the Servers module (default system module)
	mod, err := h.state.Store.GetModuleByID(id)
	if err != nil {
		sendJSONError(w, "Module not found", 404)
		return
	}
	if mod.IsSystem && !req.IsEnabled {
		sendJSONError(w, mod.Name+" module cannot be disabled", 400)
		return
	}

	if err := h.state.Store.UpdateModuleStatus(id, req.IsEnabled); err != nil {
		sendJSONError(w, "Update failed", 500)
		return
	}
	h.state.Events.Publish(r.Context(), "modules.changed", nil)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
