package store

import (
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestResolveNodeEnrollToken_Valid(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT user_id, platform FROM node_enroll_tokens
			 WHERE token_hash = $1 AND consumed_at IS NULL AND recovers_node_token IS NULL
			   AND (expires_at IS NULL OR expires_at > NOW())`)).
		WithArgs(hashAuthToken("plain-token")).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "platform"}).AddRow("user-1", false))

	userID, platform, ok, err := s.ResolveNodeEnrollToken("plain-token")
	if err != nil {
		t.Fatalf("ResolveNodeEnrollToken: %v", err)
	}
	if !ok || userID != "user-1" || platform {
		t.Fatalf("got (%q, %v, %v), want (user-1, false, true)", userID, platform, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestResolveNodeEnrollToken_UnknownExpiredOrRecovery pins that ANY of
// unknown / expired / consumed / recovery-scoped tokens collapse to
// ok=false, err=nil (never a raw sql.ErrNoRows leaking to the caller) —
// this is the guard that stops a recovery token being redeemed as a
// generic new-node enroll token.
func TestResolveNodeEnrollToken_UnknownExpiredOrRecovery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT user_id, platform FROM node_enroll_tokens
			 WHERE token_hash = $1 AND consumed_at IS NULL AND recovers_node_token IS NULL
			   AND (expires_at IS NULL OR expires_at > NOW())`)).
		WithArgs(hashAuthToken("bad-token")).
		WillReturnError(sql.ErrNoRows)

	userID, _, ok, err := s.ResolveNodeEnrollToken("bad-token")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if ok || userID != "" {
		t.Fatalf("got (%q, %v), want (\"\", false)", userID, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestResolveNodeEnrollToken_QueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	boom := errors.New("connection reset")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT user_id, platform FROM node_enroll_tokens
			 WHERE token_hash = $1 AND consumed_at IS NULL AND recovers_node_token IS NULL
			   AND (expires_at IS NULL OR expires_at > NOW())`)).
		WithArgs(hashAuthToken("some-token")).
		WillReturnError(boom)

	_, _, ok, err := s.ResolveNodeEnrollToken("some-token")
	if ok {
		t.Fatalf("expected ok=false on error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestConsumeNodeEnrollToken_ValidEnrollToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE node_enroll_tokens SET consumed_at = NOW()
			 WHERE token_hash = $1 AND consumed_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())
			   AND (NOT platform OR EXISTS (SELECT 1 FROM warp_api_keys k
			        WHERE k.node_id = node_enroll_tokens.warp_key_node_id
			          AND k.owner_id IS NULL AND k.revoked_at IS NULL))
			 RETURNING user_id, COALESCE(recovers_node_token, '')`)).
		WithArgs(hashAuthToken("plain-token")).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "recovers_node_token"}).AddRow("user-1", ""))

	userID, recovers, ok, err := s.ConsumeNodeEnrollToken("plain-token")
	if err != nil {
		t.Fatalf("ConsumeNodeEnrollToken: %v", err)
	}
	if !ok || userID != "user-1" || recovers != "" {
		t.Fatalf("got (%q, %q, %v), want (user-1, \"\", true)", userID, recovers, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestConsumeNodeEnrollToken_ValidRecoveryToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE node_enroll_tokens SET consumed_at = NOW()
			 WHERE token_hash = $1 AND consumed_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())
			   AND (NOT platform OR EXISTS (SELECT 1 FROM warp_api_keys k
			        WHERE k.node_id = node_enroll_tokens.warp_key_node_id
			          AND k.owner_id IS NULL AND k.revoked_at IS NULL))
			 RETURNING user_id, COALESCE(recovers_node_token, '')`)).
		WithArgs(hashAuthToken("recovery-token")).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "recovers_node_token"}).AddRow("user-1", "node-tok-abc"))

	userID, recovers, ok, err := s.ConsumeNodeEnrollToken("recovery-token")
	if err != nil {
		t.Fatalf("ConsumeNodeEnrollToken: %v", err)
	}
	if !ok || userID != "user-1" || recovers != "node-tok-abc" {
		t.Fatalf("got (%q, %q, %v), want (user-1, node-tok-abc, true)", userID, recovers, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestConsumeNodeEnrollToken_AlreadyConsumedOrExpired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE node_enroll_tokens SET consumed_at = NOW()
			 WHERE token_hash = $1 AND consumed_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())
			   AND (NOT platform OR EXISTS (SELECT 1 FROM warp_api_keys k
			        WHERE k.node_id = node_enroll_tokens.warp_key_node_id
			          AND k.owner_id IS NULL AND k.revoked_at IS NULL))
			 RETURNING user_id, COALESCE(recovers_node_token, '')`)).
		WithArgs(hashAuthToken("used-token")).
		WillReturnError(sql.ErrNoRows)

	userID, recovers, ok, err := s.ConsumeNodeEnrollToken("used-token")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if ok || userID != "" || recovers != "" {
		t.Fatalf("got (%q, %q, %v), want (\"\", \"\", false)", userID, recovers, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestResolveRecoveryToken_ValidRecoveryToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(recovers_node_token, '') FROM node_enroll_tokens
			 WHERE token_hash = $1 AND consumed_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())`)).
		WithArgs(hashAuthToken("recovery-token")).
		WillReturnRows(sqlmock.NewRows([]string{"recovers_node_token"}).AddRow("node-tok-abc"))

	recovers, ok, err := s.ResolveRecoveryToken("recovery-token")
	if err != nil {
		t.Fatalf("ResolveRecoveryToken: %v", err)
	}
	if !ok || recovers != "node-tok-abc" {
		t.Fatalf("got (%q, %v), want (node-tok-abc, true)", recovers, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestResolveRecoveryToken_PlainEnrollTokenRejected pins the post-scan
// branch: a row IS found (token valid/unexpired/unconsumed) but
// recovers_node_token is empty — a plain enroll token, not a recovery
// token — so the method still reports ok=false. This is the logic that
// can't be caught by a SQL-only test; it's the in-Go guard after Scan.
func TestResolveRecoveryToken_PlainEnrollTokenRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(recovers_node_token, '') FROM node_enroll_tokens
			 WHERE token_hash = $1 AND consumed_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())`)).
		WithArgs(hashAuthToken("plain-enroll-token")).
		WillReturnRows(sqlmock.NewRows([]string{"recovers_node_token"}).AddRow(""))

	recovers, ok, err := s.ResolveRecoveryToken("plain-enroll-token")
	if err != nil {
		t.Fatalf("ResolveRecoveryToken: %v", err)
	}
	if ok || recovers != "" {
		t.Fatalf("got (%q, %v), want (\"\", false)", recovers, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestResolveRecoveryToken_UnknownOrExpired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(recovers_node_token, '') FROM node_enroll_tokens
			 WHERE token_hash = $1 AND consumed_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())`)).
		WithArgs(hashAuthToken("unknown-token")).
		WillReturnError(sql.ErrNoRows)

	recovers, ok, err := s.ResolveRecoveryToken("unknown-token")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if ok || recovers != "" {
		t.Fatalf("got (%q, %v), want (\"\", false)", recovers, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
