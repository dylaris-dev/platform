package users

import (
	"encoding/json"
	"net/http"

	"dylaris-core/handlers/httpx"
)

type AccountPolicyHandler struct {
	d Deps
}

func NewAccountPolicyHandler(d Deps) *AccountPolicyHandler {
	return &AccountPolicyHandler{d: d}
}

type accountPolicy struct {
	AllowNameChange        bool `json:"allowNameChange"`
	NameChangeCooldownDays int  `json:"nameChangeCooldownDays"`
}

// Get GET /api/admin/settings/users - PANEL settings.read (RequireCap at the route).
func (h *AccountPolicyHandler) Get(w http.ResponseWriter, r *http.Request) {
	allow, days, _ := h.d.Store.GetUserAccountPolicy()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"policy":  accountPolicy{AllowNameChange: allow, NameChangeCooldownDays: days},
	})
}

// Set PUT /api/admin/settings/users - PANEL settings.write (RequireCap at the route).
func (h *AccountPolicyHandler) Set(w http.ResponseWriter, r *http.Request) {
	var req accountPolicy
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.NameChangeCooldownDays < 0 || req.NameChangeCooldownDays > 3650 {
		httpx.SendJSONError(w, "Cooldown must be 0-3650 days", http.StatusBadRequest)
		return
	}
	if err := h.d.Store.SetUserAccountPolicy(req.AllowNameChange, req.NameChangeCooldownDays); err != nil {
		httpx.SendJSONError(w, "Save failed", http.StatusInternalServerError)
		return
	}
	h.d.Events.Publish(r.Context(), "users.policy.changed", nil)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
