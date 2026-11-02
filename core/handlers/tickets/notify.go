package tickets

import (
	"log"

	"dylaris-core/models"
	"dylaris-core/store"
)

// Notification types - kept here as constants so producers + consumers share
// one vocabulary. New types added in later phases append here.
const (
	NotifyTypeTicketReply      = "ticket_reply"
	NotifyTypeTicketStatus     = "ticket_status"
	NotifyTypeTicketAssigned   = "ticket_assigned"
	NotifyTypeTicketWatcherAdd = "ticket_watcher_added"
	NotifyTypeTicketAutoClosed = "ticket_auto_closed"
	NotifyTypeTicketOpened     = "ticket_opened"
)

// emitTicketNotification creates one notification row per recipient. Callers
// pass the actor (excluded from fan-out - you don't get notified for your
// own action) and a small slice of recipient user IDs.
//
// Best-effort: each insert error is logged but does not block the originating
// request. Returns silently when the store is nil so tests can skip wiring.
func emitTicketNotification(st store.Store, recipients []string, kind string, title, body, link string) {
	if st == nil || len(recipients) == 0 {
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
		if _, err := st.InsertNotification(n); err != nil {
			log.Printf("notify: insert for user %s failed: %v", uid, err)
		}
	}
}
