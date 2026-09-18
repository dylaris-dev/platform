package store

import (
	"database/sql"
	"dylaris-core/models"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

func TestCreateShareLink_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO share_links
		(build_id, kind, token, expires_at, created_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`)).
		WithArgs(42, models.ShareLinkClientMrpack, "tok-abc", nil, "user-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(9))

	l := &models.ShareLink{
		BuildID:   42,
		Kind:      models.ShareLinkClientMrpack,
		Token:     "tok-abc",
		CreatedBy: "user-1",
	}
	id, err := s.CreateShareLink(l)
	if err != nil {
		t.Fatalf("CreateShareLink: %v", err)
	}
	if id != 9 {
		t.Fatalf("got id %d, want 9", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestCreateShareLink_WithExpiry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	exp := time.Now().Add(24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO share_links
		(build_id, kind, token, expires_at, created_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`)).
		WithArgs(42, models.ShareLinkServerPack, "tok-xyz", exp, "user-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(10))

	l := &models.ShareLink{
		BuildID:   42,
		Kind:      models.ShareLinkServerPack,
		Token:     "tok-xyz",
		CreatedBy: "user-1",
		ExpiresAt: &exp,
	}
	id, err := s.CreateShareLink(l)
	if err != nil {
		t.Fatalf("CreateShareLink: %v", err)
	}
	if id != 10 {
		t.Fatalf("got id %d, want 10", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCreateShareLink_TokenCollision documents CURRENT behavior: unlike
// RenameUser/CreateAdditionalAdmin, CreateShareLink does NOT map a 23505
// unique-violation (share_links_token_uniq) to a sentinel — the raw
// *pq.Error propagates to the caller as-is. Not changed here (production
// logic is out of scope); flagged in the wave report.
func TestCreateShareLink_TokenCollision(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO share_links
		(build_id, kind, token, expires_at, created_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`)).
		WithArgs(42, models.ShareLinkClientMrpack, "dup-token", nil, "user-1").
		WillReturnError(&pq.Error{Code: "23505"})

	l := &models.ShareLink{
		BuildID:   42,
		Kind:      models.ShareLinkClientMrpack,
		Token:     "dup-token",
		CreatedBy: "user-1",
	}
	_, err = s.CreateShareLink(l)
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) || pqErr.Code != "23505" {
		t.Fatalf("got %v, want raw *pq.Error{Code: 23505}", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestGetShareLinkByToken_Found(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + shareLinkCols + `
		FROM share_links WHERE token=$1`)).
		WithArgs("tok-abc").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "build_id", "kind", "token", "expires_at", "created_by", "created_at", "revoked",
		}).AddRow(9, 42, models.ShareLinkClientMrpack, "tok-abc", nil, "user-1", now, false))

	l, err := s.GetShareLinkByToken("tok-abc")
	if err != nil {
		t.Fatalf("GetShareLinkByToken: %v", err)
	}
	if l == nil || l.ID != 9 || l.Revoked || l.ExpiresAt != nil {
		t.Fatalf("unexpected link: %+v", l)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestGetShareLinkByToken_ReturnsRevokedAndExpired pins that the store does
// NOT filter revoked/expired rows itself — it returns the row verbatim and
// leaves the revoked/expiry check to the caller (handlers.PacksHandler.ServeShare
// checks link.Revoked and link.ExpiresAt after the fetch). Confirms the
// nullable expires_at -> *time.Time mapping when a value IS present too.
func TestGetShareLinkByToken_ReturnsRevokedAndExpired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	past := time.Now().Add(-24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + shareLinkCols + `
		FROM share_links WHERE token=$1`)).
		WithArgs("tok-old").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "build_id", "kind", "token", "expires_at", "created_by", "created_at", "revoked",
		}).AddRow(3, 1, models.ShareLinkServerPack, "tok-old", past, "user-1", past, true))

	l, err := s.GetShareLinkByToken("tok-old")
	if err != nil {
		t.Fatalf("GetShareLinkByToken: %v", err)
	}
	if l == nil || !l.Revoked {
		t.Fatalf("expected a revoked link back (store does not filter), got %+v", l)
	}
	if l.ExpiresAt == nil || !l.ExpiresAt.Equal(past) {
		t.Fatalf("expected ExpiresAt %v, got %v", past, l.ExpiresAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestGetShareLinkByToken_NotFound pins the nil,nil-on-no-match contract
// so the handler's uniform
// 404 works off a nil link, not an error.
func TestGetShareLinkByToken_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + shareLinkCols + `
		FROM share_links WHERE token=$1`)).
		WithArgs("unknown").
		WillReturnError(sql.ErrNoRows)

	l, err := s.GetShareLinkByToken("unknown")
	if err != nil {
		t.Fatalf("expected nil error on no match, got %v", err)
	}
	if l != nil {
		t.Fatalf("expected nil link, got %+v", l)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListShareLinksByBuild_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + shareLinkCols + `
		FROM share_links WHERE build_id=$1 ORDER BY created_at DESC`)).
		WithArgs(42).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "build_id", "kind", "token", "expires_at", "created_by", "created_at", "revoked",
		}).
			AddRow(2, 42, models.ShareLinkServerPack, "tok-2", nil, "user-1", now, true).
			AddRow(1, 42, models.ShareLinkClientMrpack, "tok-1", nil, "user-1", now, false))

	out, err := s.ListShareLinksByBuild(42)
	if err != nil {
		t.Fatalf("ListShareLinksByBuild: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 links, got %d", len(out))
	}
	if out[0].ID != 2 || !out[0].Revoked || out[1].ID != 1 || out[1].Revoked {
		t.Fatalf("unexpected links: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListShareLinksByBuild_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + shareLinkCols + `
		FROM share_links WHERE build_id=$1 ORDER BY created_at DESC`)).
		WithArgs(99).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "build_id", "kind", "token", "expires_at", "created_by", "created_at", "revoked",
		}))

	out, err := s.ListShareLinksByBuild(99)
	if err != nil {
		t.Fatalf("ListShareLinksByBuild: %v", err)
	}
	if out == nil || len(out) != 0 {
		t.Fatalf("expected empty (non-nil) slice, got %#v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestRevokeShareLink_HappyPath pins the owner-scoped, idempotency-guarded
// UPDATE (revoked=FALSE in the WHERE so a double-revoke is caught below).
func TestRevokeShareLink_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE share_links SET revoked=TRUE
		WHERE id=$1 AND created_by=$2 AND revoked=FALSE`)).
		WithArgs(9, "user-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.RevokeShareLink(9, "user-1"); err != nil {
		t.Fatalf("RevokeShareLink: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestRevokeShareLink_NotFoundOrNotOwner pins the zero-rows-affected branch:
// wrong owner, unknown id, or an already-revoked link all collapse to the
// same sql.ErrNoRows sentinel (0 rows matched WHERE) so the handler can 404
// uniformly without a separate ownership check.
func TestRevokeShareLink_NotFoundOrNotOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE share_links SET revoked=TRUE
		WHERE id=$1 AND created_by=$2 AND revoked=FALSE`)).
		WithArgs(9, "someone-else").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err = s.RevokeShareLink(9, "someone-else")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("got %v, want sql.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestRevokeShareLink_ExecError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	boom := errors.New("connection reset")
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE share_links SET revoked=TRUE
		WHERE id=$1 AND created_by=$2 AND revoked=FALSE`)).
		WithArgs(9, "user-1").
		WillReturnError(boom)

	err = s.RevokeShareLink(9, "user-1")
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
