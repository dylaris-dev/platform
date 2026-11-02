package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"dylaris-core/authz"
	"dylaris-core/handlers/access"
	"dylaris-core/handlers/httpx"
	"dylaris-core/store"
)

// SetPermissionsMode validates the enum then persists it. Errors on an unknown
// mode rather than silently coercing, so a bad admin write is surfaced.
func SetPermissionsMode(st store.Store, mode string) error {
	if !authz.ValidMode(mode) {
		return errors.New("invalid permissions_mode: " + mode)
	}
	return st.SetSetting(authz.PermissionsModeSettingKey, mode)
}

// PermissionsModeHandler exposes the level-2 delegation mode over HTTP. GetMode
// is cap-free (any authed user, incl. owners, reads it to render /access);
// SetMode is admin/settings.write only. Both wrap the package-level helpers.
type PermissionsModeHandler struct {
	state *AppState
}

func NewPermissionsModeHandler(state *AppState) *PermissionsModeHandler {
	return &PermissionsModeHandler{state: state}
}

// GetMode GET /api/authz/mode -> {"success":true,"mode":"off|simple|advanced"}.
func (h *PermissionsModeHandler) GetMode(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		httpx.SendJSONError(w, "Database not connected", 503)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"mode":    access.PermissionsMode(h.state.Store),
	})
}

type setPermissionsModeRequest struct {
	Mode string `json:"mode"`
}

// SetMode PUT /api/admin/settings/permissions-mode <- {"mode":"..."}.
func (h *PermissionsModeHandler) SetMode(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		httpx.SendJSONError(w, "Database not connected", 503)
		return
	}
	var req setPermissionsModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", 400)
		return
	}
	if err := SetPermissionsMode(h.state.Store, req.Mode); err != nil {
		httpx.SendJSONError(w, err.Error(), 400)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
