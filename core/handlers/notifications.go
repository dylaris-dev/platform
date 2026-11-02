package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"dylaris-core/handlers/httpx"
	"dylaris-core/models"

	"github.com/gorilla/mux"
)

type NotificationsHandler struct {
	state *AppState
}

func NewNotificationsHandler(state *AppState) *NotificationsHandler {
	return &NotificationsHandler{state: state}
}

// ── Endpoints ────────────────────────────────────────────────────────

// List GET /api/notifications - the caller's own notifications plus the unread
// count. ?unread_only=1 drops the read ones and ?limit caps the list.
func (h *NotificationsHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		httpx.SendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	includeRead := r.URL.Query().Get("unread_only") != "1"
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.state.Store.ListNotifications(userID, includeRead, limit)
	if err != nil {
		httpx.SendJSONError(w, "Failed to load notifications", http.StatusInternalServerError)
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
		httpx.SendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
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
		httpx.SendJSONError(w, "Invalid id", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.MarkNotificationRead(id, userID); err != nil {
		httpx.SendJSONError(w, "Update failed", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// MarkAllRead POST /api/notifications/read-all - marks every one of the
// caller's notifications read.
func (h *NotificationsHandler) MarkAllRead(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if err := h.state.Store.MarkAllNotificationsRead(userID); err != nil {
		httpx.SendJSONError(w, "Update failed", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// Delete removes one inbox notification belonging to the caller. Reading and
// dismissal are independent, so an availability alert remains until removed.
func (h *NotificationsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value("userID").(string)
	if user == "" {
		httpx.SendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil || id <= 0 {
		httpx.SendJSONError(w, "Invalid id", 400)
		return
	}
	if err = h.state.Store.DeleteNotification(id, user); err != nil {
		httpx.SendJSONError(w, "Delete failed", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ── Emitter helper used by other handlers ────────────────────────────
