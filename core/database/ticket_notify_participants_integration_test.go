package database

import (
	"testing"

	"dylaris-core/models"
)

// Against a real Postgres, because what is being checked IS the SQL: the UNION
// that decides who hears about a ticket.
//
// The missing branch was ticket_messages, and it is what made the whole ticket
// system silent towards support. A supporter who ANSWERS a ticket is not its
// creator, is not assigned to it unless somebody assigned it, and is not a
// watcher - so the customer's next reply notified nobody. Measured on
// production: a customer replied to a ticket the admin had answered and the
// admin's bell stayed at zero.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationTicketNotifyIncludesEveryoneWhoWroteOnIt(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	// CreateUser does not write the role column - a role is assigned after the
	// fact, which is also how the panel does it.
	supporter := &models.User{Username: uniqueName("u_sup_"), Password: "x", Email: uniqueName("e_sup_") + "@example.test"}
	if err := st.CreateUser(supporter); err != nil {
		t.Fatalf("CreateUser supporter: %v", err)
	}
	if err := st.SetUserRole(supporter.ID, "support"); err != nil {
		t.Fatalf("SetUserRole: %v", err)
	}
	bystander := &models.User{Username: uniqueName("u_by_"), Password: "x", Email: uniqueName("e_by_") + "@example.test"}
	if err := st.CreateUser(bystander); err != nil {
		t.Fatalf("CreateUser bystander: %v", err)
	}
	t.Cleanup(func() {
		st.DeleteUser(supporter.ID)
		st.DeleteUser(bystander.ID)
	})

	catID, err := st.CreateTicketCategory(&models.TicketCategory{Name: uniqueName("c_"), Enabled: true, DefaultPriority: "normal"})
	if err != nil {
		t.Fatalf("CreateTicketCategory: %v", err)
	}
	ticketID, err := st.CreateTicket(&models.Ticket{
		CategoryID: catID, CategoryName: "c", UserID: f.user.ID,
		Title: "Invoice is missing", Status: "open", Priority: "normal",
	})
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	t.Cleanup(func() { st.DeleteTicket(ticketID) })

	// The customer's opening message, then the supporter's answer. Nobody has
	// been assigned and nobody is watching, which is the ordinary case.
	for _, m := range []*models.TicketMessage{
		{TicketID: ticketID, UserID: f.user.ID, Body: "I never got the invoice"},
		{TicketID: ticketID, UserID: supporter.ID, Body: "Sending it now"},
	} {
		if _, err := st.AddTicketMessage(m); err != nil {
			t.Fatalf("AddTicketMessage: %v", err)
		}
	}

	// The customer replies again: the supporter has to hear about it.
	got, err := st.ListTicketParticipantsForNotify(ticketID, f.user.ID)
	if err != nil {
		t.Fatalf("ListTicketParticipantsForNotify: %v", err)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen[supporter.ID] {
		t.Errorf("the supporter who answered is not in %v - their reply is the only reason they are on this ticket, and without them a customer's follow-up reaches nobody", got)
	}
	if seen[f.user.ID] {
		t.Errorf("the actor was notified of their own reply: %v", got)
	}
	if seen[bystander.ID] {
		t.Errorf("a user with nothing to do with the ticket was notified: %v", got)
	}

	// And the other direction: the supporter replying tells the customer.
	got, err = st.ListTicketParticipantsForNotify(ticketID, supporter.ID)
	if err != nil {
		t.Fatalf("ListTicketParticipantsForNotify: %v", err)
	}
	if len(got) != 1 || got[0] != f.user.ID {
		t.Errorf("recipients of a supporter's reply = %v, want just the customer %q", got, f.user.ID)
	}
}

// The other half of the fix, and it is a different query: who a NEW ticket is
// announced to. Nothing announced one before, so there was no list at all.
func TestIntegrationTicketStaffIsAdminsAndSupport(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	// Two ways of being staff, and both have to answer: is_admin true (how the
	// first admin and every promoted admin is stored) and role = support.
	admin := &models.User{Username: uniqueName("u_adm_"), Password: "x", Email: uniqueName("e_adm_") + "@example.test", IsAdmin: true}
	supporter := &models.User{Username: uniqueName("u_sup_"), Password: "x", Email: uniqueName("e_sup_") + "@example.test"}
	for _, u := range []*models.User{admin, supporter} {
		if err := st.CreateUser(u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		id := u.ID
		t.Cleanup(func() { st.DeleteUser(id) })
	}
	if err := st.SetUserRole(supporter.ID, "support"); err != nil {
		t.Fatalf("SetUserRole: %v", err)
	}

	got, err := st.ListTicketStaffIDs()
	if err != nil {
		t.Fatalf("ListTicketStaffIDs: %v", err)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen[admin.ID] {
		t.Errorf("an admin is not staff: %v", got)
	}
	if !seen[supporter.ID] {
		t.Errorf("a supporter is not staff: %v", got)
	}
	if seen[f.user.ID] {
		t.Errorf("an ordinary user is counted as staff and would be told about every ticket anyone opens: %v", got)
	}
}
