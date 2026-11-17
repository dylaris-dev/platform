package store

import (
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestEnrollPeerTx_FixedIPTaken pins the race-safe graceful rejection: a caller-pinned
// fixed IP that is already allocated returns ErrWarpIPTaken (not a raw wg_ip UNIQUE
// violation), and no INSERT is attempted.
func TestEnrollPeerTx_FixedIPTaken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`SELECT pg_advisory_xact_lock($1)`)).
		WithArgs(warpEnrollLock).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(warpEnrollKeyLiveSQL)).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM warp_peers WHERE api_key_id = $1`)).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM warp_peers WHERE wg_ip = $1`)).
		WithArgs("10.0.99.7").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectRollback()

	_, _, err = s.EnrollPeerTx(1, 5, "block", "pubA", "10.0.99.7", "leader-01",
		func(map[string]bool) (string, error) { return "", nil })
	if !errors.Is(err, ErrWarpIPTaken) {
		t.Fatalf("EnrollPeerTx with a taken fixed IP: got %v, want ErrWarpIPTaken", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// The enroll handler checks the key once, in its middleware, and a revoke can
// land before the insert: the revoke's teardown then lists the key's peers
// before the new row exists, and a revoked key keeps a fresh tunnel. The
// transaction re-reads the key and holds it FOR SHARE, so the revoke waits for
// the insert (and then finds the row) or wins and the enroll is refused.
func TestEnrollPeerTx_RevokedKeyInsertsNothing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`SELECT pg_advisory_xact_lock($1)`)).
		WithArgs(warpEnrollLock).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(warpEnrollKeyLiveSQL)).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"one"}))
	mock.ExpectRollback()

	_, _, err = s.EnrollPeerTx(1, 5, "block", "pubA", "", "leader-01",
		func(map[string]bool) (string, error) { return "10.0.99.2", nil })
	if !errors.Is(err, ErrWarpKeyRevoked) {
		t.Fatalf("EnrollPeerTx with a revoked key: got %v, want ErrWarpKeyRevoked", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
