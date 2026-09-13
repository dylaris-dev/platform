package store

import (
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

const createFirstAdminQ = `
		WITH guard AS (
			SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM users WHERE is_admin = true)
		)
		INSERT INTO users (id, username, password, is_admin, role, totp_secret, created_at, email_verified_at)
		SELECT gen_random_uuid(), $1, $2, true, 'admin', $3, NOW(), NOW()
		FROM guard
		RETURNING id, username, is_admin, role, totp_secret, created_at
	`

const createAdditionalAdminQ = `
		INSERT INTO users (id, username, password, is_admin, role, totp_secret, created_at, email_verified_at)
		VALUES (gen_random_uuid(), $1, $2, true, 'admin', $3, NOW(), NOW())
		RETURNING id, username, is_admin, role, totp_secret, created_at
	`

func TestCreateFirstAdmin_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	rows := sqlmock.NewRows([]string{"id", "username", "is_admin", "role", "totp_secret", "created_at"}).
		AddRow("uuid-1", "alice", true, "admin", "", time.Now())
	mock.ExpectQuery(regexp.QuoteMeta(createFirstAdminQ)).
		WithArgs("alice", "hash", "").
		WillReturnRows(rows)

	u, err := s.CreateFirstAdmin("alice", "hash", "")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	if u.Username != "alice" || !u.IsAdmin || u.Role != "admin" {
		t.Fatalf("unexpected user: %+v", u)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestCreateFirstAdmin_AlreadyComplete(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	// The guarded CTE inserts zero rows when an admin already exists -> ErrNoRows.
	mock.ExpectQuery(regexp.QuoteMeta(createFirstAdminQ)).
		WithArgs("alice", "hash", "").
		WillReturnError(sql.ErrNoRows)

	_, err = s.CreateFirstAdmin("alice", "hash", "")
	if !errors.Is(err, ErrSetupAlreadyComplete) {
		t.Fatalf("got %v, want ErrSetupAlreadyComplete", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestCreateAdditionalAdmin_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	rows := sqlmock.NewRows([]string{"id", "username", "is_admin", "role", "totp_secret", "created_at"}).
		AddRow("uuid-2", "backup-admin", true, "admin", "", time.Now())
	mock.ExpectQuery(regexp.QuoteMeta(createAdditionalAdminQ)).
		WithArgs("backup-admin", "hash", "").
		WillReturnRows(rows)

	u, err := s.CreateAdditionalAdmin("backup-admin", "hash", "")
	if err != nil {
		t.Fatalf("CreateAdditionalAdmin: %v", err)
	}
	if u.Username != "backup-admin" || !u.IsAdmin || u.Role != "admin" {
		t.Fatalf("unexpected user: %+v", u)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestCreateAdditionalAdmin_UsernameTaken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(createAdditionalAdminQ)).
		WithArgs("alice", "hash", "").
		WillReturnError(&pq.Error{Code: "23505"})

	_, err = s.CreateAdditionalAdmin("alice", "hash", "")
	if !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("got %v, want ErrUsernameTaken", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
