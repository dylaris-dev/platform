package store

import (
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A list that drives a DECISION must never come back short with a nil error.
// Round 23 fixed the rows.Err() half of this across the decision-driving paths;
// these two loops had a second hole as well - a failed Scan did `continue`, so
// one unreadable row silently deleted an entry from an otherwise complete
// answer.
//
// What makes these two the sharp ones is the consumer. An omission is not read
// as "unknown" but as "does not exist", and acted on:
//
//	ListServersByNode -> the node's Redis ACL (resetkeys: omission == revocation)
//	                  -> the disk analysis (omission == ORPHANED, offer to delete)
//	ListNodes         -> the discovery offline sweep and the rebalance worker
//
// See TestOmittedUUIDIsARevocationNotASkip in services/redisacl for the half of
// the coupling that lives on the other side.

var errRowStreamCut = errors.New("connection reset while streaming rows")

func TestListServersByNodeReportsACutStream(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM servers")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "uuid", "name", "status"}).
			AddRow(1, "uuid-a", "alpha", "online").
			RowError(0, errRowStreamCut))

	servers, err := NewPostgresStore(db).ListServersByNode(7)
	if err == nil {
		t.Fatalf("a cut stream returned %d servers and a nil error; the ACL reconciler would revoke the missing servers and the disk analysis would offer their data for deletion", len(servers))
	}
}

// A row that cannot be scanned is the likelier failure of the two, and the
// nastier one: unlike a dropped connection it is not transient, so the same
// server vanishes from every sweep for as long as the row stays unreadable.
func TestListServersByNodeReportsAnUnreadableRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// id is scanned into an int; a non-numeric value fails Scan on that row.
	mock.ExpectQuery(regexp.QuoteMeta("FROM servers")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "uuid", "name", "status"}).
			AddRow(1, "uuid-a", "alpha", "online").
			AddRow("not-an-int", "uuid-b", "bravo", "online"))

	servers, err := NewPostgresStore(db).ListServersByNode(7)
	if err == nil {
		t.Fatalf("an unreadable row returned %d servers and a nil error; uuid-b would be treated as a server that does not exist", len(servers))
	}
}

func TestListNodesReportsACutStream(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM nodes")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1).RowError(0, errRowStreamCut))

	nodes, err := NewPostgresStore(db).ListNodes()
	if err == nil {
		t.Fatalf("a cut stream returned %d nodes and a nil error; the discovery sweep would stop reasoning about the rest of the fleet", len(nodes))
	}
}

func TestListNodesReportsAnUnreadableRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// One column against a 24-column scan target: the scan fails on this row.
	mock.ExpectQuery(regexp.QuoteMeta("FROM nodes")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))

	nodes, err := NewPostgresStore(db).ListNodes()
	if err == nil {
		t.Fatalf("an unreadable row returned %d nodes and a nil error", len(nodes))
	}
}
