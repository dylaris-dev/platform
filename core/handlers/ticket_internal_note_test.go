package handlers

import (
	"errors"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// internalNoteStore answers the two lookups the fan-out makes.
type internalNoteStore struct {
	store.Store

	watchers    []models.TicketWatcher
	watchersErr error
	users       map[string]*models.User
}

func (f *internalNoteStore) ListTicketWatchers(int) ([]models.TicketWatcher, error) {
	return f.watchers, f.watchersErr
}
func (f *internalNoteStore) GetUserByID(id string) (*models.User, error) {
	u, ok := f.users[id]
	if !ok {
		return nil, errors.New("no such user")
	}
	return u, nil
}

const (
	noteActor    = "aaaaaaaa-0000-4000-8000-000000000001" // the supporter writing the note
	noteAssignee = "bbbbbbbb-0000-4000-8000-000000000002"
	noteStaffCC  = "cccccccc-0000-4000-8000-000000000003"
	noteUserCC   = "dddddddd-0000-4000-8000-000000000004" // the customer, CC'd
	noteAdminCC  = "eeeeeeee-0000-4000-8000-000000000005"
)

func internalNoteFixture() (*AppState, *models.Ticket, *internalNoteStore) {
	assignee := noteAssignee
	fs := &internalNoteStore{
		watchers: []models.TicketWatcher{
			{UserID: noteUserCC},
			{UserID: noteStaffCC},
			{UserID: noteAdminCC},
			{UserID: noteActor},    // the actor is also a watcher
			{UserID: noteAssignee}, // already covered by the assignee branch
		},
		users: map[string]*models.User{
			noteActor:    {ID: noteActor, Role: "support"},
			noteAssignee: {ID: noteAssignee, Role: "support"},
			noteStaffCC:  {ID: noteStaffCC, Role: "support"},
			noteUserCC:   {ID: noteUserCC, Role: "user"},
			noteAdminCC:  {ID: noteAdminCC, IsAdmin: true},
		},
	}
	return &AppState{Store: fs}, &models.Ticket{ID: 7, UserID: noteUserCC, AssignedUserID: &assignee}, fs
}

// An internal note is staff-only. The customer must never be told one exists,
// even when they are a watcher on their own ticket - which they always are in
// spirit, since it is their ticket.
func TestAnInternalNoteNeverReachesANonStaffWatcher(t *testing.T) {
	state, ticket, _ := internalNoteFixture()

	got := internalNoteRecipients(state, ticket, ticket.ID, noteActor)

	set := map[string]bool{}
	for _, id := range got {
		if set[id] {
			t.Errorf("recipient %s listed twice", id)
		}
		set[id] = true
	}
	if set[noteUserCC] {
		t.Error("the customer was told an internal note exists")
	}
	if set[noteActor] {
		t.Error("the author was notified about their own note")
	}
	if !set[noteAssignee] {
		t.Error("the assignee was not notified")
	}
	if !set[noteStaffCC] {
		t.Error("a support watcher was not notified - the fan-out is back to assignee-only")
	}
	if !set[noteAdminCC] {
		t.Error("an admin watcher was not notified")
	}
	if len(got) != 3 {
		t.Errorf("recipients = %v, want exactly the assignee and the two staff watchers", got)
	}
}

// A watcher lookup that fails must not swallow the notification for the
// assignee too.
func TestAWatcherLookupFailureStillNotifiesTheAssignee(t *testing.T) {
	state, ticket, fs := internalNoteFixture()
	fs.watchersErr = errors.New("connection reset by peer")

	got := internalNoteRecipients(state, ticket, ticket.ID, noteActor)

	if len(got) != 1 || got[0] != noteAssignee {
		t.Errorf("recipients = %v, want just the assignee", got)
	}
}

// An unassigned ticket with no staff watchers notifies nobody, rather than
// falling back to anyone reachable.
func TestAnUnassignedTicketWithNoStaffWatchersNotifiesNobody(t *testing.T) {
	state, ticket, fs := internalNoteFixture()
	ticket.AssignedUserID = nil
	fs.watchers = []models.TicketWatcher{{UserID: noteUserCC}}

	if got := internalNoteRecipients(state, ticket, ticket.ID, noteActor); len(got) != 0 {
		t.Errorf("recipients = %v, want none", got)
	}
}
