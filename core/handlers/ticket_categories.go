package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/models"

	"github.com/gorilla/mux"
)

type TicketCategoriesHandler struct {
	state *AppState
}

func NewTicketCategoriesHandler(state *AppState) *TicketCategoriesHandler {
	return &TicketCategoriesHandler{state: state}
}

// validTicketPriority is the closed set of accepted priorities. New values
// must be added here AND in the matching JSON-schema-like checks on the
// client; keep the two in sync.
func validTicketPriority(p string) bool {
	return p == "low" || p == "normal" || p == "high" || p == "urgent"
}

// ListCategories GET /api/ticket-categories — any auth.
// Returns only enabled categories — used by the create-ticket form.
func (h *TicketCategoriesHandler) ListCategories(w http.ResponseWriter, r *http.Request) {
	out, err := h.state.Store.ListTicketCategories(false)
	if err != nil {
		sendJSONError(w, "Failed to load categories", http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []models.TicketCategory{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"categories": out,
	})
}

// AdminListCategories GET /api/admin/ticket-categories - RequireCap("tickets.read") at the route.
// Includes disabled categories — used by the admin management UI.
func (h *TicketCategoriesHandler) AdminListCategories(w http.ResponseWriter, r *http.Request) {
	out, err := h.state.Store.ListTicketCategories(true)
	if err != nil {
		sendJSONError(w, "Failed to load categories", http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []models.TicketCategory{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"categories": out,
	})
}

type categoryRequest struct {
	Name                string `json:"name"`
	Description         string `json:"description"`
	RequiresServer      bool   `json:"requiresServer"`
	DefaultPriority     string `json:"defaultPriority"`
	DefaultAssigneeTeam string `json:"defaultAssigneeTeam"`
	Color               string `json:"color"`
	Enabled             *bool  `json:"enabled,omitempty"`
	Position            int    `json:"position"`
}

func (req *categoryRequest) toModel(existing *models.TicketCategory) (*models.TicketCategory, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, &simpleErr{msg: "Name is required"}
	}
	if len(name) > 128 {
		return nil, &simpleErr{msg: "Name is too long (max 128 chars)"}
	}
	priority := strings.TrimSpace(req.DefaultPriority)
	if priority == "" {
		priority = "normal"
	}
	if !validTicketPriority(priority) {
		return nil, &simpleErr{msg: "Invalid default priority"}
	}
	c := models.TicketCategory{
		Name:                name,
		Description:         strings.TrimSpace(req.Description),
		RequiresServer:      req.RequiresServer,
		DefaultPriority:     priority,
		DefaultAssigneeTeam: strings.TrimSpace(req.DefaultAssigneeTeam),
		Color:               strings.TrimSpace(req.Color),
		Enabled:             true,
		Position:            req.Position,
	}
	if existing != nil {
		c.ID = existing.ID
		// Default to keeping existing enabled state when caller omits it.
		c.Enabled = existing.Enabled
	}
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	return &c, nil
}

// CreateCategory POST /api/admin/ticket-categories - RequireCap("tickets.write") at the route.
func (h *TicketCategoriesHandler) CreateCategory(w http.ResponseWriter, r *http.Request) {
	var req categoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	c, err := req.toModel(nil)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := h.state.Store.CreateTicketCategory(c)
	if err != nil {
		sendJSONError(w, "Failed to create (name may already exist)", http.StatusConflict)
		return
	}
	created, _ := h.state.Store.GetTicketCategory(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"category": created,
	})
}

// UpdateCategory PATCH /api/admin/ticket-categories/{id} - RequireCap("tickets.write") at the route.
func (h *TicketCategoriesHandler) UpdateCategory(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		sendJSONError(w, "Invalid id", http.StatusBadRequest)
		return
	}
	existing, err := h.state.Store.GetTicketCategory(id)
	if err != nil || existing == nil {
		sendJSONError(w, "Category not found", http.StatusNotFound)
		return
	}
	var req categoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	updated, err := req.toModel(existing)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.state.Store.UpdateTicketCategory(updated); err != nil {
		sendJSONError(w, "Update failed", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"category": updated,
	})
}

// DeleteCategory DELETE /api/admin/ticket-categories/{id} - RequireCap("tickets.delete") at the route.
// Refuses when the category has tickets attached (FK is ON DELETE RESTRICT).
// Caller should soft-disable (enabled=FALSE) for categories with history.
func (h *TicketCategoriesHandler) DeleteCategory(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		sendJSONError(w, "Invalid id", http.StatusBadRequest)
		return
	}
	// Deleting is allowed even with tickets attached. Each ticket carries the
	// category NAME it was filed under, so nothing loses its label; the id goes
	// NULL and the ticket stops being filterable by a category that no longer
	// exists. Refusing used to be the protection, and it made a typo made in the
	// first five minutes of an install permanent.
	if err := h.state.Store.DeleteTicketCategory(id); err != nil {
		sendJSONError(w, "Could not delete the category", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
