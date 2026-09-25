package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// The identity audit trail records what happens to ACCOUNTS: registrations,
// email and username changes, role and permission changes, 2FA resets, and
// deletions.
//
// It was written and never read. ListAuditIdentity had no caller outside the
// store and its tests, there was no route, and no screen - so the record could
// only be reached with a database shell. That included the deletion row added
// after an account was removed and nobody could answer what had been in it,
// which was the entire point of adding it.
//
// This is also what audit.read finally gates. The capability was in the catalog
// and seeded into the "support" panel role while no route in the tree checked
// it, so granting it conferred nothing.
type IdentityAuditHandler struct {
	state *AppState
}

func NewIdentityAuditHandler(state *AppState) *IdentityAuditHandler {
	return &IdentityAuditHandler{state: state}
}

// identityAuditRow is the wire shape: the stored event plus the names its two
// user ids resolve to, so a reader does not have to hold the user list.
type identityAuditRow struct {
	ID         int64                  `json:"id"`
	EventType  string                 `json:"eventType"`
	ActorID    string                 `json:"actorUserId,omitempty"`
	ActorName  string                 `json:"actorName,omitempty"`
	TargetID   string                 `json:"targetUserId,omitempty"`
	TargetName string                 `json:"targetName,omitempty"`
	Metadata   map[string]interface{} `json:"metadata,omitempty"`
	IPAddress  string                 `json:"ipAddress,omitempty"`
	UserAgent  string                 `json:"userAgent,omitempty"`
	CreatedAt  string                 `json:"createdAt"`
}

// List GET /api/admin/audit/identity?eventType=&targetUserId=&limit=
func (h *IdentityAuditHandler) List(w http.ResponseWriter, r *http.Request) {
	if h.state == nil || h.state.Store == nil {
		sendJSONError(w, "Database not connected", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var target *string
	if t := q.Get("targetUserId"); t != "" {
		target = &t
	}
	events, err := h.state.Store.ListAuditIdentity(target, q.Get("eventType"), limit)
	if err != nil {
		sendJSONError(w, "Failed to read the identity log", http.StatusInternalServerError)
		return
	}

	// One pass over the user list rather than a lookup per row. An id that
	// resolves to nothing is left blank on purpose: the accounts most worth
	// reading about here are the DELETED ones, and their identity lives in the
	// row's own metadata, which is the only place it can survive them.
	names := map[string]string{}
	if users, uerr := h.state.Store.ListUsers(); uerr == nil {
		for _, u := range users {
			names[u.ID] = u.Username
		}
	}

	out := make([]identityAuditRow, 0, len(events))
	for _, ev := range events {
		row := identityAuditRow{
			ID:        ev.ID,
			EventType: ev.EventType,
			Metadata:  ev.Metadata,
			IPAddress: ev.IPAddress,
			UserAgent: ev.UserAgent,
			CreatedAt: ev.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		}
		if ev.ActorUserID != nil {
			row.ActorID = *ev.ActorUserID
			row.ActorName = names[*ev.ActorUserID]
		}
		if ev.TargetUserID != nil {
			row.TargetID = *ev.TargetUserID
			row.TargetName = names[*ev.TargetUserID]
		}
		out = append(out, row)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "events": out})
}
