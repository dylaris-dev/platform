package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"dylaris-core/models"

	"github.com/gorilla/mux"
)

type NotificationsHandler struct {
	state *AppState
}

func NewNotificationsHandler(state *AppState) *NotificationsHandler {
	return &NotificationsHandler{state: state}
}

// Notification types — kept here as constants so producers + consumers share
// one vocabulary. New types added in later phases append here.
const (
	NotifyTypeTicketReply      = "ticket_reply"
	NotifyTypeTicketStatus     = "ticket_status"
	NotifyTypeTicketAssigned   = "ticket_assigned"
	NotifyTypeTicketWatcherAdd = "ticket_watcher_added"
	NotifyTypeTicketAutoClosed = "ticket_auto_closed"
	NotifyTypeTicketOpened     = "ticket_opened"
)

// ── Endpoints ────────────────────────────────────────────────────────

// List GET /api/notifications - the caller's own notifications plus the unread
// count. ?unread_only=1 drops the read ones and ?limit caps the list.
func (h *NotificationsHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	includeRead := r.URL.Query().Get("unread_only") != "1"
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.state.Store.ListNotifications(userID, includeRead, limit)
	if err != nil {
		sendJSONError(w, "Failed to load notifications", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []models.Notification{}
	}
	unread, _ := h.state.Store.CountUnreadNotifications(userID)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"notifications": list,
		"unread":        unread,
	})
}

// UnreadCount GET /api/notifications/unread-count — cheap polling endpoint
// for the bell badge.
func (h *NotificationsHandler) UnreadCount(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	n, _ := h.state.Store.CountUnreadNotifications(userID)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"unread":  n,
	})
}

// MarkRead POST /api/notifications/{id}/read - marks one notification read.
// The update is scoped by user id, so it cannot mark someone else's.
func (h *NotificationsHandler) MarkRead(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil || id <= 0 {
		sendJSONError(w, "Invalid id", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.MarkNotificationRead(id, userID); err != nil {
		sendJSONError(w, "Update failed", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// MarkAllRead POST /api/notifications/read-all - marks every one of the
// caller's notifications read.
func (h *NotificationsHandler) MarkAllRead(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if err := h.state.Store.MarkAllNotificationsRead(userID); err != nil {
		sendJSONError(w, "Update failed", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ── Emitter helper used by other handlers ────────────────────────────

// EmitTicketNotification creates one notification row per recipient. Callers
// pass the actor (excluded from fan-out — you don't get notified for your
// own action) and a small slice of recipient user IDs.
//
// Best-effort: each insert error is logged but does not block the originating
// request. Returns silently when state/store are nil so tests can skip wiring.
func EmitTicketNotification(state *AppState, recipients []string, kind string, title, body, link string) {
	if state == nil || state.Store == nil || len(recipients) == 0 {
		return
	}
	for _, uid := range recipients {
		n := &models.Notification{
			UserID: uid,
			Type:   kind,
			Title:  title,
			Body:   body,
			Link:   link,
		}
		if _, err := state.Store.InsertNotification(n); err != nil {
			log.Printf("notify: insert for user %s failed: %v", uid, err)
		}
	}
}
