package templates

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"dylaris-core/authz"
	"dylaris-core/handlers/httpx"
	"dylaris-core/models"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

type shareRequest struct {
	Username string `json:"username"`
	Level    string `json:"level"`
}

// shareTarget decodes {username, level} and resolves the user. Sharing with an
// unknown user, the caller or the owner is a 400.
func (h *TemplatesHandler) shareTarget(w http.ResponseWriter, r *http.Request, callerID, ownerID string) (*models.User, string, bool) {
	if !httpx.CapBody(w, r, 16<<10) {
		return nil, "", false
	}
	var p shareRequest
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return nil, "", false
	}
	if p.Level != "use" && p.Level != "edit" {
		httpx.SendJSONError(w, "Level must be use or edit", http.StatusBadRequest)
		return nil, "", false
	}
	name := strings.TrimSpace(p.Username)
	if name == "" {
		httpx.SendJSONError(w, "Username is required", http.StatusBadRequest)
		return nil, "", false
	}
	// One answer for unknown, owner and self, so the form does not double as a
	// username lookup beyond what it has to.
	u, err := h.state.Store.GetUserByUsername(name)
	if err != nil || u == nil || u.ID == callerID || u.ID == ownerID {
		httpx.SendJSONError(w, "Cannot share with that user. Check the username; you cannot share with yourself or the owner.", http.StatusBadRequest)
		return nil, "", false
	}
	return u, p.Level, true
}

// ListShares GET /api/templates/{id}/shares
func (h *TemplatesHandler) ListShares(w http.ResponseWriter, r *http.Request) {
	t, _, _, ok := h.load(w, r, needShare)
	if !ok {
		return
	}
	shares, err := h.state.Store.ListTemplateShares(t.ID)
	if err != nil {
		log.Printf("templates: shares of %d: %v", t.ID, err)
		httpx.SendJSONError(w, "Could not list the shares", http.StatusInternalServerError)
		return
	}
	if shares == nil {
		shares = []models.TemplateShare{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"shares": shares})
}

// PutShare PUT /api/templates/{id}/shares - {username, level}; sets or changes one share.
func (h *TemplatesHandler) PutShare(w http.ResponseWriter, r *http.Request) {
	t, v, _, ok := h.load(w, r, needShare)
	if !ok {
		return
	}
	u, level, ok := h.shareTarget(w, r, v.UserID, t.OwnerID)
	if !ok {
		return
	}
	if err := h.state.Store.UpsertTemplateShare(t.ID, u.ID, level); err != nil {
		log.Printf("templates: share %d with %s: %v", t.ID, u.ID, err)
		httpx.SendJSONError(w, "Could not save the share", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "userId": u.ID, "username": u.Username, "level": level})
}

// DeleteShare DELETE /api/templates/{id}/shares/{userId}
func (h *TemplatesHandler) DeleteShare(w http.ResponseWriter, r *http.Request) {
	t, _, _, ok := h.load(w, r, needShare)
	if !ok {
		return
	}
	target := mux.Vars(r)["userId"]
	if _, err := uuid.Parse(target); err != nil {
		httpx.SendJSONError(w, "Invalid user ID", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.DeleteTemplateShare(t.ID, target); err != nil {
		log.Printf("templates: unshare %d: %v", t.ID, err)
		httpx.SendJSONError(w, "Could not remove the share", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// ListOwnerShares GET /api/templates/owner-shares - who the caller shares all
// their personal templates with.
func (h *TemplatesHandler) ListOwnerShares(w http.ResponseWriter, r *http.Request) {
	id := authz.IdentityFromContext(r.Context())
	shares, err := h.state.Store.ListOwnerShares(id.UserID)
	if err != nil {
		log.Printf("templates: owner shares of %s: %v", id.UserID, err)
		httpx.SendJSONError(w, "Could not list the shares", http.StatusInternalServerError)
		return
	}
	if shares == nil {
		shares = []models.TemplateShare{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"shares": shares})
}

// PutOwnerShare PUT /api/templates/owner-shares - {username, level}; shares
// all of the caller's personal templates, present and future.
func (h *TemplatesHandler) PutOwnerShare(w http.ResponseWriter, r *http.Request) {
	id := authz.IdentityFromContext(r.Context())
	u, level, ok := h.shareTarget(w, r, id.UserID, id.UserID)
	if !ok {
		return
	}
	if err := h.state.Store.UpsertOwnerShare(id.UserID, u.ID, level); err != nil {
		log.Printf("templates: owner share %s with %s: %v", id.UserID, u.ID, err)
		httpx.SendJSONError(w, "Could not save the share", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "userId": u.ID, "username": u.Username, "level": level})
}

// DeleteOwnerShare DELETE /api/templates/owner-shares?userId=
func (h *TemplatesHandler) DeleteOwnerShare(w http.ResponseWriter, r *http.Request) {
	id := authz.IdentityFromContext(r.Context())
	target := r.URL.Query().Get("userId")
	if _, err := uuid.Parse(target); err != nil {
		httpx.SendJSONError(w, "Invalid user ID", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.DeleteOwnerShare(id.UserID, target); err != nil {
		log.Printf("templates: owner unshare %s: %v", id.UserID, err)
		httpx.SendJSONError(w, "Could not remove the share", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}
