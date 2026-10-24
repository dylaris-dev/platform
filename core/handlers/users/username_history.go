package users

import (
	"encoding/json"
	"net/http"
	"strings"

	"dylaris-core/handlers"

	"dylaris-pkg/validate"
)

type UsernameHistoryHandler struct {
	state *handlers.AppState
}

func NewUsernameHistoryHandler(state *handlers.AppState) *UsernameHistoryHandler {
	return &UsernameHistoryHandler{state: state}
}

// Me GET /api/me/username-history - the calling user's own past usernames.
func (h *UsernameHistoryHandler) Me(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		handlers.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	rows, err := h.state.Store.ListUsernameHistory(userID)
	if err != nil {
		handlers.SendJSONError(w, "Failed to load history", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "history": rows})
}

// Admin GET /api/admin/users/{id}/username-history
// Gated at the route with RequireCap("users.read"); admin short-circuits.
func (h *UsernameHistoryHandler) Admin(w http.ResponseWriter, r *http.Request) {
	targetID, ok := handlers.ParseUserID(w, r)
	if !ok {
		return
	}
	rows, err := h.state.Store.ListUsernameHistory(targetID)
	if err != nil {
		handlers.SendJSONError(w, "Failed to load history", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "history": rows})
}

// AdminRename PATCH /api/admin/users/{id}/username
// Body: {"username": "newname"} — bypasses cooldown + platform toggle.
// Gated at the route with RequireCap("users.write"); admin short-circuits.
func (h *UsernameHistoryHandler) AdminRename(w http.ResponseWriter, r *http.Request) {
	adminID, _ := r.Context().Value("userID").(string)
	if adminID == "" {
		handlers.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	targetID, ok := handlers.ParseUserID(w, r)
	if !ok {
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		handlers.SendJSONError(w, "username required", http.StatusBadRequest)
		return
	}
	// Same charset guard as the self-service rename: a username is interpolated
	// into Redis keys, so ':'/space must be rejected here too.
	if !validate.IsUsername(req.Username) {
		handlers.SendJSONError(w, "Invalid username: 3-32 characters, must start with a letter or digit, then letters, digits, '.', '_' or '-'", http.StatusBadRequest)
		return
	}
	// Uniqueness check
	// Case-INSENSITIVE; the unique index on LOWER(username) is the real guard.
	target, terr := h.state.Store.GetUserByID(targetID)
	if terr != nil || target == nil {
		handlers.SendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	if !handlers.MayManageAccount(h.state, r, target) {
		handlers.SendJSONError(w, "You cannot rename an account with more rights than yours", http.StatusForbidden)
		return
	}
	// Through the admin route a staff member's own rename skipped the cooldown
	// the profile applies to everyone else.
	if !handlers.IsAdmin(r) && handlers.ActingOnSelf(r, targetID) {
		handlers.SendJSONError(w, "Change your own username from your profile", http.StatusForbidden)
		return
	}
	if taken, _ := h.state.Store.UsernameTaken(req.Username, targetID); taken {
		handlers.SendJSONError(w, "Username already taken", http.StatusConflict)
		return
	}
	if err := h.state.Store.RenameUser(targetID, req.Username, adminID); err != nil {
		handlers.SendJSONError(w, "Rename failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "users.changed", map[string]interface{}{"userId": targetID})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
