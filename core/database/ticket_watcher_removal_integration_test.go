package database

import (
	"testing"

	"dylaris-core/models"
)

// RemoveTicketWatcher used to return nil whether or not a row matched, and the
// handler wrote its audit event on the strength of that nil.
//
// The consequence was not academic. The permission check lets a caller act on
// their OWN watcher row - "a watcher may remove themselves" - without asking
// whether they are a watcher at all, so any authenticated account could send
// DELETE /api/tickets/<n>/watchers/<their own id> against a ticket belonging to
// a stranger. The delete matched nothing, the handler answered 200, and an
// audit line naming them appeared in a support ticket they have no relation to.
// Counting upwards through <n> also answered which ticket numbers exist.
//
// Both follow from one thing the store now reports: whether a row went away.
func TestIntegrationRemoveTicketWatcherReportsWhetherARowWent(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	cat := &models.TicketCategory{Name: uniqueName("cat_"), Enabled: true, DefaultPriority: "normal"}
	catID, err := st.CreateTicketCategory(cat)
	if err != nil {
		t.Fatalf("CreateTicketCategory: %v", err)
	}
	t.Cleanup(func() { st.DeleteTicketCategory(catID) })

	ticketID, err := st.CreateTicket(&models.Ticket{
		CategoryID: catID, UserID: f.user.ID, Title: "watchers", Status: "open", Priority: "normal",
	})
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}

	stranger := &models.User{
		Username: uniqueName("stranger_"),
		Password: "x",
		Email:    uniqueName("stranger_") + "@example.test",
	}
	if err := st.CreateUser(stranger); err != nil {
		t.Fatalf("CreateUser(stranger): %v", err)
	}
	t.Cleanup(func() { st.DeleteUser(stranger.ID) })

	// The exploited call: someone who is not a watcher, on a ticket that is not
	// theirs, naming themselves.
	removed, err := st.RemoveTicketWatcher(ticketID, stranger.ID)
	if err != nil {
		t.Fatalf("RemoveTicketWatcher for a non-watcher: %v", err)
	}
	if removed {
		t.Error("removed = true for a user who was never a watcher")
	}

	// A ticket number that does not exist has to look the same, or the handler
	// still answers whether it exists.
	removed, err = st.RemoveTicketWatcher(ticketID+900000, stranger.ID)
	if err != nil {
		t.Fatalf("RemoveTicketWatcher on an absent ticket: %v", err)
	}
	if removed {
		t.Error("removed = true for a ticket that does not exist")
	}

	// And the real removal still reports itself, or the fix would have turned
	// every legitimate call into a 404.
	if inserted, err := st.AddTicketWatcher(&models.TicketWatcher{
		TicketID: ticketID, UserID: stranger.ID, CanReply: false,
	}); err != nil || !inserted {
		t.Fatalf("AddTicketWatcher: inserted=%v err=%v, want a new row", inserted, err)
	}
	// Adding the same watcher again is an update. It used to notify the
	// watcher on every call, which made it a way to flood any user.
	if inserted, err := st.AddTicketWatcher(&models.TicketWatcher{
		TicketID: ticketID, UserID: stranger.ID, CanReply: true,
	}); err != nil || inserted {
		t.Fatalf("AddTicketWatcher again: inserted=%v err=%v, want an update", inserted, err)
	}
	removed, err = st.RemoveTicketWatcher(ticketID, stranger.ID)
	if err != nil {
		t.Fatalf("RemoveTicketWatcher for a real watcher: %v", err)
	}
	if !removed {
		t.Error("removed = false after removing an actual watcher")
	}

	// Twice in a row is the same non-removal as the first case.
	removed, err = st.RemoveTicketWatcher(ticketID, stranger.ID)
	if err != nil {
		t.Fatalf("RemoveTicketWatcher a second time: %v", err)
	}
	if removed {
		t.Error("removed = true on the second removal of the same watcher")
	}
}
