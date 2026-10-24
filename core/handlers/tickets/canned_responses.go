package tickets

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/handlers"

	"dylaris-core/models"

	"github.com/gorilla/mux"
)

type CannedResponsesHandler struct {
	state *handlers.AppState
}

func NewCannedResponsesHandler(state *handlers.AppState) *CannedResponsesHandler {
	return &CannedResponsesHandler{state: state}
}

// ListForSupport GET /api/ticket-canned-responses
// Available to support+admin. Filter optional ?categoryId= to limit.
func (h *CannedResponsesHandler) ListForSupport(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	perms := handlers.LoadEffectivePermissions(h.state, userID)
	if !perms.IsAdmin && !perms.IsSupport {
		handlers.SendJSONError(w, "Support or admin required", http.StatusForbidden)
		return
	}
	var categoryID *int
	if v := r.URL.Query().Get("categoryId"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			categoryID = &n
		}
	}
	list, err := h.state.Store.ListCannedResponses(categoryID)
	if err != nil {
		handlers.SendJSONError(w, "Failed to load", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []models.CannedResponse{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"responses": list,
	})
}

// AdminList GET /api/admin/ticket-canned-responses - RequireCap("tickets.read") at the route, full list.
func (h *CannedResponsesHandler) AdminList(w http.ResponseWriter, r *http.Request) {
	list, _ := h.state.Store.ListCannedResponses(nil)
	if list == nil {
		list = []models.CannedResponse{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"responses": list,
	})
}

type cannedRequest struct {
	Name       string `json:"name"`
	Body       string `json:"body"`
	CategoryID *int   `json:"categoryId,omitempty"`
}

func (req *cannedRequest) validate() (*models.CannedResponse, error) {
	name := strings.TrimSpace(req.Name)
	body := strings.TrimSpace(req.Body)
	if name == "" || len(name) > 128 {
		return nil, &simpleErr{msg: "Name is required (max 128 chars)"}
	}
	if body == "" || len(body) > 10000 {
		return nil, &simpleErr{msg: "Body must be 1-10000 characters"}
	}
	return &models.CannedResponse{
		Name:       name,
		Body:       body,
		CategoryID: req.CategoryID,
	}, nil
}

// Create POST /api/admin/ticket-canned-responses - RequireCap("tickets.write") at the route.
func (h *CannedResponsesHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req cannedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	c, err := req.validate()
	if err != nil {
		handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	actorID, _ := r.Context().Value("userID").(string)
	if actorID == "" {
		handlers.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	v := actorID
	c.CreatedBy = &v
	id, err := h.state.Store.CreateCannedResponse(c)
	if err != nil {
		handlers.SendJSONError(w, "Create failed (name may already exist)", http.StatusConflict)
		return
	}
	created, _ := h.state.Store.GetCannedResponse(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"response": created,
	})
}

// Update PATCH /api/admin/ticket-canned-responses/{id} - RequireCap("tickets.write") at the route.
func (h *CannedResponsesHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		handlers.SendJSONError(w, "Invalid id", http.StatusBadRequest)
		return
	}
	existing, _ := h.state.Store.GetCannedResponse(id)
	if existing == nil {
		handlers.SendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	var req cannedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	updated, err := req.validate()
	if err != nil {
		handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	updated.ID = existing.ID
	if err := h.state.Store.UpdateCannedResponse(updated); err != nil {
		handlers.SendJSONError(w, "Update failed", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"response": updated,
	})
}

// Delete DELETE /api/admin/ticket-canned-responses/{id} - RequireCap("tickets.delete") at the route.
func (h *CannedResponsesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		handlers.SendJSONError(w, "Invalid id", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.DeleteCannedResponse(id); err != nil {
		handlers.SendJSONError(w, "Delete failed", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// simpleErr is a validation message returned as an error; a copy of the one in
// handlers (security_questions.go), whose field is unexported.
type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }
