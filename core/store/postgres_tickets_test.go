package store

import (
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// Column order must match scanTicket in postgres_tickets.go.
var ticketRowCols = []string{
	"id", "region", "category_id", "category_name",
	"user_id", "user_name",
	"server_uuid", "server_region",
	"subject_kind", "subject_ref",
	"title", "status", "priority",
	"assigned_user_id", "assigned_name", "assigned_team",
	"created_at", "updated_at", "closed_at",
}

// TestListTickets_NoFilter pins the zero-value TicketFilter case: WHERE 1=1
// only, default LIMIT 50 OFFSET 0, no bound args beyond the base query.
func TestListTickets_NoFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	wantQuery := `SELECT ` + ticketSelectCols + ` ` + ticketBaseFrom +
		` WHERE 1=1 ORDER BY t.updated_at DESC LIMIT 50 OFFSET 0`
	mock.ExpectQuery(regexp.QuoteMeta(wantQuery)).
		WillReturnRows(sqlmock.NewRows(ticketRowCols))

	out, err := s.ListTickets(TicketFilter{})
	if err != nil {
		t.Fatalf("ListTickets: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected no rows, got %d", len(out))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestListTickets_UserIDAndStatus pins a single-scalar filter (UserID, via the
// "add" closure) combined with a slice filter (Status, via statusArrayArg +
// ANY($n)) — the two different code paths that build conds/args.
func TestListTickets_UserIDAndStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	userID := "user-1"
	wantQuery := `SELECT ` + ticketSelectCols + ` ` + ticketBaseFrom +
		` WHERE 1=1 AND t.user_id = $1 AND t.status = ANY($2) ORDER BY t.updated_at DESC LIMIT 50 OFFSET 0`
	mock.ExpectQuery(regexp.QuoteMeta(wantQuery)).
		WithArgs(userID, "{open,in_progress}").
		WillReturnRows(sqlmock.NewRows(ticketRowCols))

	out, err := s.ListTickets(TicketFilter{UserID: &userID, Status: []string{"open", "in_progress"}})
	if err != nil {
		t.Fatalf("ListTickets: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected no rows, got %d", len(out))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestListTickets_AssignedTeamPriorityCategory pins the ordering of THREE
// stacked conditions (AssignedTeam, Priority slice, CategoryID), a custom
// Limit/Offset (inlined as literals, NOT bound params), scanTicket's row
// mapping (including the nullable assigned_user_id / closed_at columns), and
// the follow-up message-count-by-ticket-id query fired only when rows come
// back.
func TestListTickets_AssignedTeamPriorityCategory(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	catID := 7
	wantQuery := `SELECT ` + ticketSelectCols + ` ` + ticketBaseFrom +
		` WHERE 1=1 AND t.assigned_team = $1 AND t.priority = ANY($2) AND t.category_id = $3` +
		` ORDER BY t.updated_at DESC LIMIT 10 OFFSET 5`

	now := time.Now()
	rows := sqlmock.NewRows(ticketRowCols).
		AddRow(5, "eu", 7, "Billing", "user-1", "alice",
			"", "", "", "", "Cannot pay", "open", "high",
			nil, "", "", now, now, nil)

	mock.ExpectQuery(regexp.QuoteMeta(wantQuery)).
		WithArgs("support-a", "{high}", catID).
		WillReturnRows(rows)

	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT ticket_id, COUNT(*) FROM ticket_messages
				 WHERE ticket_id = ANY($1) GROUP BY ticket_id`)).
		WithArgs("{5}").
		WillReturnRows(sqlmock.NewRows([]string{"ticket_id", "count"}).AddRow(5, 3))

	out, err := s.ListTickets(TicketFilter{
		AssignedTeam: "support-a",
		Priority:     []string{"high"},
		CategoryID:   &catID,
		Limit:        10,
		Offset:       5,
	})
	if err != nil {
		t.Fatalf("ListTickets: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 row, got %d", len(out))
	}
	got := out[0]
	if got.ID != 5 || got.CategoryName != "Billing" || got.Username != "alice" || got.Priority != "high" {
		t.Fatalf("unexpected ticket mapping: %+v", got)
	}
	if got.AssignedUserID != nil {
		t.Fatalf("expected nil AssignedUserID, got %v", *got.AssignedUserID)
	}
	if got.ClosedAt != nil {
		t.Fatalf("expected nil ClosedAt, got %v", *got.ClosedAt)
	}
	if got.MessageCount != 3 {
		t.Fatalf("expected MessageCount 3, got %d", got.MessageCount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestListTickets_QueryError pins that a query-level error (e.g. a bad
// filter combination hitting the DB) propagates unchanged to the caller.
func TestListTickets_QueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	region := "eu"
	wantQuery := `SELECT ` + ticketSelectCols + ` ` + ticketBaseFrom +
		` WHERE 1=1 AND t.region = $1 ORDER BY t.updated_at DESC LIMIT 50 OFFSET 0`
	boom := errors.New("connection reset")
	mock.ExpectQuery(regexp.QuoteMeta(wantQuery)).
		WithArgs(region).
		WillReturnError(boom)

	_, err = s.ListTickets(TicketFilter{Region: region})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// The inbox's unassigned and team-visibility narrowings are SQL conditions, so
// LIMIT/OFFSET page the narrowed set rather than the newest tickets overall.
func TestListTickets_UnassignedAndSupportViewer(t *testing.T) {
	me := "support-1"
	cases := []struct {
		name  string
		f     TicketFilter
		where string
		args  []driver.Value
	}{
		{"unassigned", TicketFilter{Unassigned: true},
			` WHERE 1=1 AND t.assigned_user_id IS NULL AND COALESCE(t.assigned_team, '') = ''`, nil},
		{"viewer without team", TicketFilter{SupportViewer: &me},
			` WHERE 1=1 AND (t.user_id = $1 OR t.assigned_user_id = $1 OR COALESCE(t.assigned_team, '') = '')`,
			[]driver.Value{me}},
		{"viewer with team", TicketFilter{SupportViewer: &me, SupportTeam: "billing"},
			` WHERE 1=1 AND (t.user_id = $1 OR t.assigned_user_id = $1 OR COALESCE(t.assigned_team, '') = '' OR t.assigned_team = $2)`,
			[]driver.Value{me, "billing"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			s := NewPostgresStore(db)
			wantQuery := `SELECT ` + ticketSelectCols + ` ` + ticketBaseFrom + c.where +
				` ORDER BY t.updated_at DESC LIMIT 50 OFFSET 0`
			e := mock.ExpectQuery(regexp.QuoteMeta(wantQuery))
			if c.args != nil {
				e = e.WithArgs(c.args...)
			} else {
				e = e.WithoutArgs()
			}
			e.WillReturnRows(sqlmock.NewRows(ticketRowCols))
			if _, err := s.ListTickets(c.f); err != nil {
				t.Fatalf("ListTickets: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}
