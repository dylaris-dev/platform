package users

import (
	"encoding/json"
	"net/http"

	"dylaris-core/handlers"
)

type AccountPolicyHandler struct {
	state *handlers.AppState
}

func NewAccountPolicyHandler(state *handlers.AppState) *AccountPolicyHandler {
	return &AccountPolicyHandler{state: state}
}

type accountPolicy struct {
	AllowNameChange        bool `json:"allowNameChange"`
	NameChangeCooldownDays int  `json:"nameChangeCooldownDays"`
}

// Get GET /api/admin/settings/users - PANEL settings.read (RequireCap at the route).
func (h *AccountPolicyHandler) Get(w http.ResponseWriter, r *http.Request) {
	allow, days, _ := h.state.Store.GetUserAccountPolicy()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"policy":  accountPolicy{AllowNameChange: allow, NameChangeCooldownDays: days},
	})
}

// Set PUT /api/admin/settings/users - PANEL settings.write (RequireCap at the route).
func (h *AccountPolicyHandler) Set(w http.ResponseWriter, r *http.Request) {
	var req accountPolicy
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.NameChangeCooldownDays < 0 || req.NameChangeCooldownDays > 3650 {
		handlers.SendJSONError(w, "Cooldown must be 0-3650 days", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetUserAccountPolicy(req.AllowNameChange, req.NameChangeCooldownDays); err != nil {
		handlers.SendJSONError(w, "Save failed", http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "users.policy.changed", nil)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
