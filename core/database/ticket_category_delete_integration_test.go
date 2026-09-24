package database

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// Deleting a category must not touch the tickets filed under it.
//
// The category NAME is snapshotted onto the ticket, so the ticket keeps saying
// what it was filed under. Before this the FK was ON DELETE RESTRICT and the
// delete was simply refused, which made a name typed wrong in the first five
// minutes of an install permanent.
//
// The dangerous half is not the delete, it is the READ: ticketBaseFrom used to
// INNER JOIN the category, so a NULL category_id would have made the ticket
// vanish from every list while the row was still there. That is worse than the
// restriction it replaces, and it is what this test is really guarding.
func TestIntegrationDeletingACategoryKeepsItsTickets(t *testing.T) {
	db, st := integrationDB(t)

	// Unique per run, like every other integration test in this package. A
	// fixed username passes exactly once per fresh database and fails with a
	// duplicate-key error on every run after it - which CI never sees, because
	// its Postgres is thrown away each time, and which makes the test look like
	// a real failure to anyone iterating locally.
	var userID string
	if err := db.QueryRow(
		`INSERT INTO users (username, password, role) VALUES ($1, 'x', 'user') RETURNING id`,
		uniqueName("cat_del_user_"),
	).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}

	catName := uniqueName("Typo'd category ")
	catID, err := st.CreateTicketCategory(&models.TicketCategory{
		Name: catName, DefaultPriority: "normal", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create category: %v", err)
	}

	ticketID, err := st.CreateTicket(&models.Ticket{
		Region:       "eu",
		CategoryID:   catID,
		CategoryName: catName,
		UserID:       userID,
		Title:        "Something is broken",
		Status:       "open",
		Priority:     "normal",
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}

	if err := st.DeleteTicketCategory(catID); err != nil {
		t.Fatalf("the category could not be deleted, which is the whole point: %v", err)
	}

	got, err := st.GetTicket(ticketID)
	if err != nil || got == nil {
		t.Fatalf("the ticket disappeared with its category: err=%v ticket=%v", err, got)
	}
	if got.CategoryName != catName {
		t.Errorf("the ticket lost the name it was filed under: %q", got.CategoryName)
	}
	if got.CategoryID != 0 {
		t.Errorf("category_id should be NULL (read as 0) after the delete, got %d", got.CategoryID)
	}

	// The list is the part an INNER JOIN would have broken silently.
	list, err := st.ListTickets(store.TicketFilter{})
	if err != nil {
		t.Fatalf("list tickets: %v", err)
	}
	var found bool
	for _, x := range list {
		if x.ID == ticketID {
			found = true
			if x.CategoryName != catName {
				t.Errorf("the listed ticket lost its category name: %q", x.CategoryName)
			}
		}
	}
	if !found {
		t.Error("the ticket is gone from the list: a deleted category hides tickets instead of un-labelling them")
	}
}

// A category with no tickets was always deletable and must stay that way.
func TestIntegrationAnUnusedCategoryIsStillDeletable(t *testing.T) {
	_, st := integrationDB(t)

	id, err := st.CreateTicketCategory(&models.TicketCategory{
		Name: "Never used", DefaultPriority: "low", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.DeleteTicketCategory(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if c, err := st.GetTicketCategory(id); err == nil && c != nil {
		t.Error("the category is still there after a delete")
	}
}
