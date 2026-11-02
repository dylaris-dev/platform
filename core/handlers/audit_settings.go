package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"

	"dylaris-core/handlers/httpx"
)

// AuditPolicy holds the platform-wide audit settings. Today only the
// server-audit retention horizon; in future this gains identity-audit
// retention and per-event-type retention overrides.
type AuditPolicy struct {
	ServerRetentionDays int `json:"serverRetentionDays"`
}

// The default is 0 (keep forever) because that is what an unconfigured install
// actually does: ServerAuditRetentionService skips the sweep until a horizon has
// been saved, deliberately, since it deletes. The default used to read 365, so
// the Audit card showed "365 days" next to a hardcoded "Active" badge and the
// words "before the daily sweep deletes them" while no sweep would ever run -
// the panel has to display the policy in force, not a suggestion. 365 is still
// the recommended value and the card says so.
var defaultAuditPolicy = AuditPolicy{
	ServerRetentionDays: 0,
}

func LoadAuditPolicy(state *AppState) AuditPolicy {
	p := defaultAuditPolicy
	if v, _ := state.Store.GetSetting("audit.server_retention_days"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 && n <= 3650 {
			p.ServerRetentionDays = n
		}
	}
	return p
}

type AuditSettingsHandler struct {
	state *AppState
}

func NewAuditSettingsHandler(state *AppState) *AuditSettingsHandler {
	return &AuditSettingsHandler{state: state}
}

// GetPolicy GET /api/admin/settings/audit - PANEL settings.read (RequireCap at the route).
func (h *AuditSettingsHandler) GetPolicy(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"policy":  LoadAuditPolicy(h.state),
	})
}

// SavePolicy PUT /api/admin/settings/audit - PANEL settings.write (RequireCap at the route).
func (h *AuditSettingsHandler) SavePolicy(w http.ResponseWriter, r *http.Request) {
	var p AuditPolicy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if p.ServerRetentionDays < 0 {
		p.ServerRetentionDays = 0
	}
	if p.ServerRetentionDays > 3650 {
		p.ServerRetentionDays = 3650
	}
	actorID, _ := r.Context().Value("userID").(string)
	if err := h.state.Store.SetSettingBy("audit.server_retention_days", fmt.Sprintf("%d", p.ServerRetentionDays), actorID); err != nil {
		httpx.SendJSONError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"policy":  p,
	})
}
