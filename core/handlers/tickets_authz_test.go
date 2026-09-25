package handlers

import (
	"reflect"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// TestCanSeeTicket pins the GET-detail visibility gate (tickets.go). Matrix
// covers admin / owner / watcher / support(cross-team on/off: assigned-to-me,
// unassigned, team-match, team-mismatch) / plain outsider.
func TestCanSeeTicket(t *testing.T) {
	const owner = "owner-1"
	const other = "other-1"
	const supportUser = "support-1"

	cases := []struct {
		name        string
		ticket      *models.Ticket
		perms       EffectivePermissions
		userID      string
		isWatcher   bool
		settings    TicketSettings
		supportTeam string
		want        bool
	}{
		{"admin sees everything", &models.Ticket{UserID: owner}, EffectivePermissions{IsAdmin: true}, other, false, TicketSettings{}, "", true},
		{"owner sees their own ticket", &models.Ticket{UserID: owner}, EffectivePermissions{}, owner, false, TicketSettings{}, "", true},
		{"watcher always sees it", &models.Ticket{UserID: owner}, EffectivePermissions{}, other, true, TicketSettings{}, "", true},
		{"plain outsider denied", &models.Ticket{UserID: owner}, EffectivePermissions{}, other, false, TicketSettings{}, "", false},

		{"support with cross-team visibility sees everything", &models.Ticket{UserID: owner, AssignedTeam: "billing"}, EffectivePermissions{IsSupport: true}, supportUser, false, TicketSettings{CrossTeamVisibility: true}, "sales", true},

		{"support, cross-team off, assigned to me", &models.Ticket{UserID: owner, AssignedUserID: strPtrTickets(supportUser)}, EffectivePermissions{IsSupport: true}, supportUser, false, TicketSettings{CrossTeamVisibility: false}, "sales", true},

		{"support, cross-team off, unassigned is visible for triage", &models.Ticket{UserID: owner, AssignedTeam: ""}, EffectivePermissions{IsSupport: true}, supportUser, false, TicketSettings{CrossTeamVisibility: false}, "sales", true},

		{"support, cross-team off, assigned to other but team matches", &models.Ticket{UserID: owner, AssignedUserID: strPtrTickets(other), AssignedTeam: "sales"}, EffectivePermissions{IsSupport: true}, supportUser, false, TicketSettings{CrossTeamVisibility: false}, "sales", true},

		{"support, cross-team off, assigned to other and team mismatches", &models.Ticket{UserID: owner, AssignedUserID: strPtrTickets(other), AssignedTeam: "billing"}, EffectivePermissions{IsSupport: true}, supportUser, false, TicketSettings{CrossTeamVisibility: false}, "sales", false},

		{"support, cross-team off, assigned to other, empty supportTeam, has a team -> denied", &models.Ticket{UserID: owner, AssignedUserID: strPtrTickets(other), AssignedTeam: "billing"}, EffectivePermissions{IsSupport: true}, supportUser, false, TicketSettings{CrossTeamVisibility: false}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := canSeeTicket(c.ticket, c.perms, c.userID, c.isWatcher, c.settings, c.supportTeam); got != c.want {
				t.Errorf("canSeeTicket() = %v, want %v", got, c.want)
			}
		})
	}
}

func strPtrTickets(s string) *string { return &s }

// TestCanReply pins the reply gate (tickets.go), which returns
// (allowed, mayPostInternal). Internal notes are support+admin only.
func TestCanReply(t *testing.T) {
	const owner = "owner-1"
	const other = "other-1"

	cases := []struct {
		name            string
		perms           EffectivePermissions
		userID          string
		isWatcher       bool
		watcherCanReply bool
		wantAllowed     bool
		wantInternal    bool
	}{
		{"admin can reply and post internal", EffectivePermissions{IsAdmin: true}, other, false, false, true, true},
		{"support can reply and post internal", EffectivePermissions{IsSupport: true, CanManageTickets: true}, other, false, false, true, true},
		// Seeing the queue and answering in the platform's name are different
		// rights now, because they have different sources: tickets.read and
		// tickets.write. A read-only supporter reads and does not answer.
		{"read-only support may not reply", EffectivePermissions{IsSupport: true}, other, false, false, false, false},
		{"owner can reply but not internal", EffectivePermissions{}, owner, false, false, true, false},
		{"watcher with can-reply flag may reply but not internal", EffectivePermissions{}, other, true, true, true, false},
		{"watcher without can-reply flag denied", EffectivePermissions{}, other, true, false, false, false},
		{"outsider denied entirely", EffectivePermissions{}, other, false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ticket := &models.Ticket{UserID: owner}
			allowed, internal := canReply(ticket, c.perms, c.userID, c.isWatcher, c.watcherCanReply)
			if allowed != c.wantAllowed || internal != c.wantInternal {
				t.Errorf("canReply() = (%v, %v), want (%v, %v)", allowed, internal, c.wantAllowed, c.wantInternal)
			}
		})
	}
}

// TestCanMutate pins the status/priority/assignment mutation gate: admin, or
// somebody who may MANAGE tickets. Being able to see the queue is not enough.
func TestCanMutate(t *testing.T) {
	cases := []struct {
		name  string
		perms EffectivePermissions
		want  bool
	}{
		{"admin can mutate", EffectivePermissions{IsAdmin: true}, true},
		{"support can mutate", EffectivePermissions{IsSupport: true, CanManageTickets: true}, true},
		{"read-only support cannot mutate", EffectivePermissions{IsSupport: true}, false},
		{"plain user cannot mutate", EffectivePermissions{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := canMutate(c.perms); got != c.want {
				t.Errorf("canMutate() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestTicketVisibilityFilter pins the store.TicketFilter selection per caller
// role (tickets.go). Called on a zero-value handler since it touches no state.
func TestTicketVisibilityFilter(t *testing.T) {
	h := &TicketsHandler{}
	const uid = "u-1"

	t.Run("admin gets the unrestricted (zero-value) filter", func(t *testing.T) {
		got := h.ticketVisibilityFilter(uid, EffectivePermissions{IsAdmin: true}, TicketSettings{})
		if !reflect.DeepEqual(got, store.TicketFilter{}) {
			t.Errorf("got %+v, want zero-value filter", got)
		}
	})

	t.Run("support with cross-team visibility gets the unrestricted filter", func(t *testing.T) {
		got := h.ticketVisibilityFilter(uid, EffectivePermissions{IsSupport: true}, TicketSettings{CrossTeamVisibility: true})
		if !reflect.DeepEqual(got, store.TicketFilter{}) {
			t.Errorf("got %+v, want zero-value filter", got)
		}
	})

	t.Run("support without cross-team visibility falls back to own assignments", func(t *testing.T) {
		got := h.ticketVisibilityFilter(uid, EffectivePermissions{IsSupport: true}, TicketSettings{CrossTeamVisibility: false})
		if got.AssignedUserID == nil || *got.AssignedUserID != uid {
			t.Errorf("got %+v, want AssignedUserID = %q", got, uid)
		}
		if got.UserID != nil {
			t.Errorf("got %+v, want UserID unset", got)
		}
	})

	t.Run("plain user is scoped to their own tickets", func(t *testing.T) {
		got := h.ticketVisibilityFilter(uid, EffectivePermissions{}, TicketSettings{})
		if got.UserID == nil || *got.UserID != uid {
			t.Errorf("got %+v, want UserID = %q", got, uid)
		}
		if got.AssignedUserID != nil {
			t.Errorf("got %+v, want AssignedUserID unset", got)
		}
	})
}

func TestValidTicketStatus(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"open", true},
		{"in_progress", true},
		{"waiting_user", true},
		{"resolved", true},
		{"closed", true},
		{"bogus", false},
		{"", false},
		{"Open", false},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			if got := validTicketStatus(c.status); got != c.want {
				t.Errorf("validTicketStatus(%q) = %v, want %v", c.status, got, c.want)
			}
		})
	}
}

func TestValidTicketPriority(t *testing.T) {
	cases := []struct {
		priority string
		want     bool
	}{
		{"low", true},
		{"normal", true},
		{"high", true},
		{"urgent", true},
		{"critical", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.priority, func(t *testing.T) {
			if got := validTicketPriority(c.priority); got != c.want {
				t.Errorf("validTicketPriority(%q) = %v, want %v", c.priority, got, c.want)
			}
		})
	}
}

func TestParseCSV(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty returns nil", "", nil},
		{"single value", "open", []string{"open"}},
		{"multiple values", "open,closed,resolved", []string{"open", "closed", "resolved"}},
		{"whitespace around values trimmed", " open , closed ", []string{"open", "closed"}},
		{"empty segments dropped", "open,,closed", []string{"open", "closed"}},
		{"all-empty segments returns empty (non-nil) slice", " , , ", []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseCSV(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("parseCSV(%q) = %#v, want %#v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("parseCSV(%q) = %#v, want %#v", c.in, got, c.want)
				}
			}
		})
	}
}

func TestParseIntDefault(t *testing.T) {
	cases := []struct {
		name string
		in   string
		def  int
		want int
	}{
		{"empty returns default", "", 25, 25},
		{"valid integer parsed", "10", 25, 10},
		{"non-numeric falls back to default", "abc", 25, 25},
		{"negative integer parsed", "-5", 25, -5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseIntDefault(c.in, c.def); got != c.want {
				t.Errorf("parseIntDefault(%q, %d) = %d, want %d", c.in, c.def, got, c.want)
			}
		})
	}
}

// TestSanitizeAttachmentFilename pins the traversal/separator stripping and
// length cap (ticket_attachments.go). Cases are chosen to avoid overlapping
// ".."/separator runs, whose left-to-right non-overlapping ReplaceAll
// interaction is easy to get wrong by hand - each case below was traced
// against the exact three sequential strings.ReplaceAll calls in source.
func TestSanitizeAttachmentFilename(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain filename unchanged", "report.pdf", "report.pdf"},
		{"forward slash separators replaced", "a/b/c.txt", "a_b_c.txt"},
		{"single traversal via forward slash", "../secret.txt", "__secret.txt"},
		{"single traversal via backslash", "..\\secret.txt", "__secret.txt"},
		{"empty string becomes file", "", "file"},
		{"whitespace-only becomes file", "   ", "file"},
		{"leading/trailing whitespace trimmed", "  name.txt  ", "name.txt"},
		{"over-length input capped at 200 chars", strings.Repeat("a", 250), strings.Repeat("a", 200)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeAttachmentFilename(c.in); got != c.want {
				t.Errorf("sanitizeAttachmentFilename(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
