package store

import (
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A roll must be an UPDATE of the live row. Revoke-then-insert would leave two
// rows sharing one node_id, and GetWarpAPIKeyByNodeID selects on node_id with no
// revoked_at filter - so every later lookup for that machine, including the one
// warp authenticates through, would pick between them arbitrarily.
func TestRollWarpAPIKeyHash_UpdatesTheLiveRowInPlace(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE warp_api_keys SET key_hash = $2 WHERE node_id = $1 AND revoked_at IS NULL`)).
		WithArgs("node-abc", "new-hash").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.RollWarpAPIKeyHash("node-abc", "new-hash"); err != nil {
		t.Fatalf("RollWarpAPIKeyHash: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// The control that matters: an UPDATE matching nothing is not an error to
// database/sql, so without the RowsAffected check a roll against a revoked or
// unknown identity would report success and hand the caller a fresh secret that
// opens nothing.
func TestRollWarpAPIKeyHash_NoLiveRowIsErrNoRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE warp_api_keys SET key_hash = $2 WHERE node_id = $1 AND revoked_at IS NULL`)).
		WithArgs("node-revoked", "new-hash").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err = s.RollWarpAPIKeyHash("node-revoked", "new-hash")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("got %v, want sql.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// The panel renders this list as "keys waiting to be used" beside a cap that
// already excluded consumed and expired tokens. Returning every row made the
// enroll token of a machine that had ALREADY enrolled sit next to that machine's
// overlay key, so adding one machine read as two things to delete.
func TestListNodeEnrollTokens_ExcludesConsumedAndExpired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT id, user_id, label, created_at, expires_at, consumed_at
		 FROM node_enroll_tokens
		 WHERE user_id = $1 AND NOT platform AND consumed_at IS NULL
		   AND (expires_at IS NULL OR expires_at > NOW())
		 ORDER BY created_at DESC`)).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "user_id", "label", "created_at", "expires_at", "consumed_at"}))

	if _, err := s.ListNodeEnrollTokens("user-1"); err != nil {
		t.Fatalf("ListNodeEnrollTokens: %v", err)
	}
	// The whole assertion: sqlmock matches the query text, so an unmet
	// expectation here means the filter is gone from the SQL.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
