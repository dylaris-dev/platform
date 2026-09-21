package handlers

import (
	"errors"
	"sort"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// staffNotifyStore answers the staff lookup and records what was written.
type staffNotifyStore struct {
	store.Store

	staff    []string
	staffErr error
	sent     []models.Notification
}

func (f *staffNotifyStore) ListTicketStaffIDs() ([]string, error) { return f.staff, f.staffErr }
func (f *staffNotifyStore) InsertNotification(n *models.Notification) (int64, error) {
	f.sent = append(f.sent, *n)
	return int64(len(f.sent)), nil
}

func recipientsOf(sent []models.Notification) []string {
	out := make([]string, 0, len(sent))
	for _, n := range sent {
		out = append(out, n.UserID)
	}
	sort.Strings(out)
	return out
}

// A ticket that nobody is told about waits until somebody happens to open the
// inbox. Every other event on a ticket - a reply, an assignment, a CC - emits a
// notification; opening one, the event that starts a customer waiting, emitted
// none. Measured on production: a customer opened a ticket and the admin's bell
// stayed at zero.
func TestOpeningATicketTellsSupport(t *testing.T) {
	fs := &staffNotifyStore{staff: []string{"admin-1", "support-1", "author"}}
	state := &AppState{Store: fs}

	notifyTicketStaff(state, "author", 7, "Invoice is missing")

	got := recipientsOf(fs.sent)
	if len(got) != 2 || got[0] != "admin-1" || got[1] != "support-1" {
		t.Fatalf("notified %v; want admin-1 and support-1, and never the author", got)
	}
	if fs.sent[0].Type != NotifyTypeTicketOpened {
		t.Errorf("type = %q, want %q", fs.sent[0].Type, NotifyTypeTicketOpened)
	}
	if fs.sent[0].Link != "/tickets/7" {
		t.Errorf("link = %q, want the ticket it is about", fs.sent[0].Link)
	}
	if fs.sent[0].Body != "Invoice is missing" {
		t.Errorf("body = %q, want the ticket title so the row says what it is", fs.sent[0].Body)
	}
}

// An admin filing their own ticket is not news to themselves, and with one
// admin on a small platform that is the whole recipient list.
func TestOpeningATicketNotifiesNobodyWhenTheAuthorIsTheOnlyStaff(t *testing.T) {
	fs := &staffNotifyStore{staff: []string{"admin-1"}}
	notifyTicketStaff(&AppState{Store: fs}, "admin-1", 8, "Note to self")
	if len(fs.sent) != 0 {
		t.Fatalf("sent %d notification(s) to %v; want none", len(fs.sent), recipientsOf(fs.sent))
	}
}

// A database blip must not take the people already on the thread down with it:
// telling fewer people beats telling nobody.
func TestAFailedStaffLookupStillNotifiesTheParticipants(t *testing.T) {
	fs := &staffNotifyStore{staffErr: errors.New("db down")}
	got := withTicketStaff(&AppState{Store: fs}, []string{"customer-1"}, "actor")
	if len(got) != 1 || got[0] != "customer-1" {
		t.Fatalf("recipients = %v, want the participants that were already known", got)
	}
}

// Merging staff into an existing recipient list must not produce two bells for
// one event, and must still drop the actor.
func TestStaffAreMergedWithoutDuplicatesOrTheActor(t *testing.T) {
	fs := &staffNotifyStore{staff: []string{"support-1", "admin-1", "actor"}}
	got := withTicketStaff(&AppState{Store: fs}, []string{"customer-1", "support-1", "actor", ""}, "actor")
	sort.Strings(got)
	want := []string{"admin-1", "customer-1", "support-1"}
	if len(got) != len(want) {
		t.Fatalf("recipients = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recipients = %v, want %v", got, want)
		}
	}
}

// What a public reply does to the ticket's status. The resolved/closed rows are
// the ones that were missing: the reply was stored, the status stayed put, the
// ticket showed up in no open view and nobody was notified, so "this is still
// not fixed" went nowhere.
func TestReplyStatusTransition(t *testing.T) {
	cases := []struct {
		name               string
		status             string
		byOwner            bool
		byStaff            bool
		wantNext           string
		wantReopenAnnounce bool
	}{
		{"the owner reopens a resolved ticket", "resolved", true, false, "open", true},
		{"the owner reopens a closed ticket", "closed", true, false, "open", true},
		{"the owner answers a ticket that was waiting on them", "waiting_user", true, false, "in_progress", false},
		{"support picking up an open ticket takes it in progress", "open", false, true, "in_progress", false},
		{"support writing on a closed ticket does NOT reopen it", "closed", false, true, "", false},
		{"the owner writing on an open ticket changes nothing", "open", true, false, "", false},
		{"support writing on a ticket already in progress changes nothing", "in_progress", false, true, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, reopened := replyStatusTransition(c.status, c.byOwner, c.byStaff)
			if next != c.wantNext {
				t.Errorf("next = %q, want %q", next, c.wantNext)
			}
			if reopened != c.wantReopenAnnounce {
				t.Errorf("reopened = %v, want %v", reopened, c.wantReopenAnnounce)
			}
		})
	}
}
