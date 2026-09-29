package database

import (
	"fmt"
	"testing"

	"dylaris-core/models"
)

// Against a real Postgres, because what changed IS the query.
//
// Deleting a server or a ticket left every notification that pointed at it,
// and the inbox kept offering a thing that no longer existed. Measured on
// production: thirteen of them, every one a dead link. They are removed in the
// same transaction as their subject now.
//
// The part worth pinning is the boundary: "/tickets/1" must not take
// "/tickets/12" with it, and a notification about something else entirely must
// survive.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationDeletingASubjectDeletesItsNotifications(t *testing.T) {
	db, st := integrationDB(t)

	u := &models.User{Username: uniqueName("notif_"), Password: "x", Email: uniqueName("notif_") + "@example.test"}
	if err := st.CreateUser(u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { st.DeleteUser(u.ID) })

	notify := func(link string) int64 {
		t.Helper()
		id, err := st.InsertNotification(&models.Notification{UserID: u.ID, Type: "test", Title: "t", Link: link})
		if err != nil {
			t.Fatalf("InsertNotification %s: %v", link, err)
		}
		return id
	}
	exists := func(id int64) bool {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM notifications WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n == 1
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM notifications WHERE user_id = $1`, u.ID) })

	t.Run("a ticket", func(t *testing.T) {
		cat := &models.TicketCategory{Name: uniqueName("cat_"), Enabled: true, DefaultPriority: "normal"}
		catID, err := st.CreateTicketCategory(cat)
		if err != nil {
			t.Fatalf("CreateTicketCategory: %v", err)
		}
		t.Cleanup(func() { st.DeleteTicketCategory(catID) })
		ticketID, err := st.CreateTicket(&models.Ticket{CategoryID: catID, UserID: u.ID, Title: "t", Status: "open", Priority: "normal"})
		if err != nil {
			t.Fatalf("CreateTicket: %v", err)
		}

		own := notify(fmt.Sprintf("/tickets/%d", ticketID))
		below := notify(fmt.Sprintf("/tickets/%d/messages", ticketID))
		// Same leading digits, a different ticket.
		neighbour := notify(fmt.Sprintf("/tickets/%d0", ticketID))
		unrelated := notify("/servers/999999")

		if err := st.DeleteTicket(ticketID); err != nil {
			t.Fatalf("DeleteTicket: %v", err)
		}
		if exists(own) || exists(below) {
			t.Error("a notification about the deleted ticket survived it")
		}
		if !exists(neighbour) {
			t.Error("deleting one ticket took a DIFFERENT ticket's notification with it")
		}
		if !exists(unrelated) {
			t.Error("deleting a ticket took an unrelated notification with it")
		}
	})

	t.Run("a server", func(t *testing.T) {
		// The notification side does not depend on the server row existing,
		// and creating a real server needs a node; an id nobody uses exercises
		// exactly the part that changed.
		const id = 987654
		own := notify(fmt.Sprintf("/servers/%d", id))
		neighbour := notify(fmt.Sprintf("/servers/%d1", id))

		if err := st.DeleteServer(id); err != nil {
			t.Fatalf("DeleteServer: %v", err)
		}
		if exists(own) {
			t.Error("a notification about the deleted server survived it")
		}
		if !exists(neighbour) {
			t.Error("deleting one server took a DIFFERENT server's notification with it")
		}
	})
}
