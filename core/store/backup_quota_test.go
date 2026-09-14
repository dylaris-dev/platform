package store

import (
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// These pin that the two backup-storage quota queries count an abandoned run's
// confirmed archive, not only successes. A reaper-confirmed orphan is a failed
// row with a nonzero size (the node always reports a failed backup as size 0),
// so the WHERE clause must admit a failed row's `size_bytes > 0` - and only a
// failed row's: a running row's size is the node's own progress report.
// sqlmock does not execute SQL, so this verifies the query CARRIES that clause -
// a revert to success-only stops matching and fails here.

// orphanCountingClause is the fragment both quota queries must contain. Escaped
// so sqlmock treats it as a literal, not a regexp.
var orphanCountingClause = regexp.QuoteMeta("br.status = 'success' OR (br.status = 'failed' AND br.size_bytes > 0)")

func TestBackupBytesByOwner_CountsConfirmedOrphans(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(orphanCountingClause).
		WithArgs("owner-1").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(int64(4096)))

	got, err := s.BackupBytesByOwner("owner-1")
	if err != nil {
		t.Fatalf("BackupBytesByOwner: %v", err)
	}
	if got != 4096 {
		t.Errorf("bytes = %d, want 4096", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the query did not carry the orphan-counting clause: %v", err)
	}
}

func TestTenantBackupBytes_CountsConfirmedOrphans(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(orphanCountingClause).
		WillReturnRows(sqlmock.NewRows([]string{"owner_id", "sum"}).AddRow("owner-1", int64(8192)))

	got, err := s.TenantBackupBytes()
	if err != nil {
		t.Fatalf("TenantBackupBytes: %v", err)
	}
	if got["owner-1"] != 8192 {
		t.Errorf("bytes for owner-1 = %d, want 8192", got["owner-1"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the query did not carry the orphan-counting clause: %v", err)
	}
}
