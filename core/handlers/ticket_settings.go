package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"

	"dylaris-core/services"
)

// TicketSettings mirrors the tickets.* keys. Defaults are chosen so a
// freshly-enabled Ticket module is friendly:
//   - cross_team_visibility ON  → no team setup needed before the first ticket
//   - watchers_default_can_reply OFF → CC starts read-only by convention
//   - allow_users_to_add_watchers ON → users self-serve CC additions
//   - audit_retention_days 0   → keep forever until an operator picks a horizon.
//     TicketAuditRetentionService only sweeps a horizon that was actually saved,
//     so anything else here would be a number the card displays and nothing
//     enforces. 180 is the recommended value and the card says so.
type TicketSettings struct {
	CrossTeamVisibility     bool `json:"crossTeamVisibility"`
	WatchersDefaultCanReply bool `json:"watchersDefaultCanReply"`
	AllowUsersToAddWatchers bool `json:"allowUsersToAddWatchers"`
	AuditRetentionDays      int  `json:"auditRetentionDays"`

	// Attachment quotas (megabytes), on the platform limit convention: nil is no
	// cap, 0 is a real "none" (attachments not allowed), n is the cap. They used
	// to be ints documented as "0 = unlimited", and every enforcement site
	// guarded on `> 0`, so an admin who set 0 to forbid attachments switched the
	// check off instead.
	//
	// The per-file limit still meets a hard body cap regardless; see
	// ticketUploadBodyLimit.
	MaxFileSizeMB   *int64 `json:"maxFileSizeMb"`
	MaxTicketSizeMB *int64 `json:"maxTicketSizeMb"`
	MaxUserSizeMB   *int64 `json:"maxUserSizeMb"`

	// Auto-close. When enabled, resolved tickets get closed by
	// the background job after AutoCloseDaysAfterResolved days of no activity.
	AutoCloseEnabled           bool `json:"autoCloseEnabled"`
	AutoCloseDaysAfterResolved int  `json:"autoCloseDaysAfterResolved"`

	// Ticket deletion gate (admin-only DELETE /api/tickets/{id}). Default OFF:
	// the audited delete path stays dormant until the admin opts in. Flipping
	// this off again does NOT undo any prior deletions — the deletion log row
	// is permanent.
	DeletionEnabled bool `json:"deletionEnabled"`
}

var defaultTicketSettings = TicketSettings{
	CrossTeamVisibility:        true,
	WatchersDefaultCanReply:    false,
	AllowUsersToAddWatchers:    true,
	AuditRetentionDays:         0,
	MaxFileSizeMB:              services.LimitPtr(10),
	MaxTicketSizeMB:            services.LimitPtr(50),
	MaxUserSizeMB:              services.LimitPtr(500),
	AutoCloseEnabled:           false,
	AutoCloseDaysAfterResolved: 7,
	DeletionEnabled:            false,
}

func LoadTicketSettings(state *AppState) TicketSettings {
	s := defaultTicketSettings
	if v, _ := state.Store.GetSetting("tickets.cross_team_visibility"); v != "" {
		s.CrossTeamVisibility = v == "true"
	}
	if v, _ := state.Store.GetSetting("tickets.watchers_default_can_reply"); v != "" {
		s.WatchersDefaultCanReply = v == "true"
	}
	if v, _ := state.Store.GetSetting("tickets.allow_users_to_add_watchers"); v != "" {
		s.AllowUsersToAddWatchers = v == "true"
	}
	if v, _ := state.Store.GetSetting("tickets.audit_retention_days"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 && n <= 3650 {
			s.AuditRetentionDays = n
		}
	}
	if v, err := state.Store.GetSetting("tickets.max_file_size_mb"); err == nil {
		if q := services.ParseLimitSetting(v, defaultTicketSettings.MaxFileSizeMB); q == nil || *q <= 1024 {
			s.MaxFileSizeMB = q
		}
	}
	if v, err := state.Store.GetSetting("tickets.max_ticket_size_mb"); err == nil {
		if q := services.ParseLimitSetting(v, defaultTicketSettings.MaxTicketSizeMB); q == nil || *q <= 10240 {
			s.MaxTicketSizeMB = q
		}
	}
	if v, err := state.Store.GetSetting("tickets.max_user_size_mb"); err == nil {
		if q := services.ParseLimitSetting(v, defaultTicketSettings.MaxUserSizeMB); q == nil || *q <= 102400 {
			s.MaxUserSizeMB = q
		}
	}
	if v, _ := state.Store.GetSetting("tickets.auto_close_enabled"); v != "" {
		s.AutoCloseEnabled = v == "true"
	}
	if v, _ := state.Store.GetSetting("tickets.auto_close_days_after_resolved"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 1 && n <= 365 {
			s.AutoCloseDaysAfterResolved = n
		}
	}
	if v, _ := state.Store.GetSetting("tickets.deletion_enabled"); v != "" {
		s.DeletionEnabled = v == "true"
	}
	return s
}

type TicketSettingsHandler struct {
	state *AppState
}

func NewTicketSettingsHandler(state *AppState) *TicketSettingsHandler {
	return &TicketSettingsHandler{state: state}
}

// GetSettings GET /api/admin/settings/tickets - RequireCap("tickets.read") at the route.
func (h *TicketSettingsHandler) GetSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"settings": LoadTicketSettings(h.state),
	})
}

// SaveSettings PUT /api/admin/settings/tickets - RequireCap("tickets.write") at the route.
func (h *TicketSettingsHandler) SaveSettings(w http.ResponseWriter, r *http.Request) {
	// The operator's policy, not a support task. tickets.write is what the
	// support role holds, and this screen turns off the team isolation that
	// support is held inside, shortens the retention of the audit trail that
	// watches support (1 day erased it on the next sweep), lifts the
	// attachment quotas and switches on deletion. Same boundary the ticket
	// migration tools already draw.
	if !IsAdmin(r) {
		sendJSONError(w, "Only an administrator can change the ticket settings", http.StatusForbidden)
		return
	}
	var s TicketSettings
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if s.AuditRetentionDays < 0 {
		s.AuditRetentionDays = 0
	}
	if s.AuditRetentionDays > 3650 {
		s.AuditRetentionDays = 3650
	}
	// A negative quota is clamped to 0, which now means "no attachments" rather
	// than the old "unlimited". nil is left alone: it IS the no-cap answer.
	for _, q := range []**int64{&s.MaxFileSizeMB, &s.MaxTicketSizeMB, &s.MaxUserSizeMB} {
		if *q != nil && **q < 0 {
			*q = services.LimitPtr(0)
		}
	}
	if s.AutoCloseDaysAfterResolved < 1 {
		s.AutoCloseDaysAfterResolved = 1
	}
	if s.AutoCloseDaysAfterResolved > 365 {
		s.AutoCloseDaysAfterResolved = 365
	}
	actorID, _ := r.Context().Value("userID").(string)
	pairs := []struct{ k, v string }{
		{"tickets.cross_team_visibility", fmt.Sprintf("%t", s.CrossTeamVisibility)},
		{"tickets.watchers_default_can_reply", fmt.Sprintf("%t", s.WatchersDefaultCanReply)},
		{"tickets.allow_users_to_add_watchers", fmt.Sprintf("%t", s.AllowUsersToAddWatchers)},
		{"tickets.audit_retention_days", fmt.Sprintf("%d", s.AuditRetentionDays)},
		{"tickets.max_file_size_mb", services.FormatLimitSetting(s.MaxFileSizeMB)},
		{"tickets.max_ticket_size_mb", services.FormatLimitSetting(s.MaxTicketSizeMB)},
		{"tickets.max_user_size_mb", services.FormatLimitSetting(s.MaxUserSizeMB)},
		{"tickets.auto_close_enabled", fmt.Sprintf("%t", s.AutoCloseEnabled)},
		{"tickets.auto_close_days_after_resolved", fmt.Sprintf("%d", s.AutoCloseDaysAfterResolved)},
		{"tickets.deletion_enabled", fmt.Sprintf("%t", s.DeletionEnabled)},
	}
	for _, kv := range pairs {
		if err := h.state.Store.SetSettingBy(kv.k, kv.v, actorID); err != nil {
			sendJSONError(w, "Failed to save: "+kv.k, http.StatusInternalServerError)
			return
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"settings": s,
	})
}
