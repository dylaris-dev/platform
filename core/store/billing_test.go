package store

import (
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestGetUserBilling_Found(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	now := time.Now()
	grace := now.Add(48 * time.Hour)
	rows := sqlmock.NewRows([]string{
		"user_id", "status", "grace_until", "suspended_at", "grace_period",
		"r2_retention", "node_retention", "r2_quota_gb", "max_nodes", "max_links",
		"traffic_edge_gb", "traffic_relay_gb", "traffic_combined_gb",
		"manual_entitlement", "manual_entitlement_expires_at",
		"manual_entitlement_byon_expires_at", "manual_entitlement_route_expires_at",
		"manual_entitlement_granted_at", "manual_entitlement_granted_by",
		"overlimit_since", "traffic_ceiling_gb", "traffic_billing_enabled", "backup_billing_enabled", "admin_hold", "held_status", "node_links_off", "updated_at",
	}).AddRow("user-1", "past_due", grace, nil, "48h", "", "", nil, 5, nil, nil, nil, nil, "", nil, nil, nil, nil, nil, nil, int64(0), false, false, false, "", false, now)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + userBillingCols + ` FROM user_billing WHERE user_id = $1`)).
		WithArgs("user-1").
		WillReturnRows(rows)

	b, err := s.GetUserBilling("user-1")
	if err != nil {
		t.Fatalf("GetUserBilling: %v", err)
	}
	if b.Status != "past_due" || b.GraceUntil == nil || !b.GraceUntil.Equal(grace) {
		t.Fatalf("unexpected billing row: %+v", b)
	}
	if b.MaxNodes == nil || *b.MaxNodes != 5 {
		t.Fatalf("expected MaxNodes override 5, got %v", b.MaxNodes)
	}
	if b.MaxLinks != nil {
		t.Fatalf("expected nil MaxLinks (no override), got %v", *b.MaxLinks)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestGetUserBilling_NoRowDefaultsActive pins the "missing row means active,
// no overrides" contract: sql.ErrNoRows is swallowed into a zero-value
// UserBilling{Status: "active"}, NOT propagated as an error.
func TestGetUserBilling_NoRowDefaultsActive(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + userBillingCols + ` FROM user_billing WHERE user_id = $1`)).
		WithArgs("user-2").
		WillReturnError(sql.ErrNoRows)

	b, err := s.GetUserBilling("user-2")
	if err != nil {
		t.Fatalf("expected nil error on no row, got %v", err)
	}
	if b == nil || b.UserID != "user-2" || b.Status != "active" {
		t.Fatalf("expected default active row, got %+v", b)
	}
	if b.GraceUntil != nil || b.MaxNodes != nil {
		t.Fatalf("expected zero-value overrides, got %+v", b)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestGetUserBilling_QueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	boom := errors.New("connection reset")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + userBillingCols + ` FROM user_billing WHERE user_id = $1`)).
		WithArgs("user-3").
		WillReturnError(boom)

	_, err = s.GetUserBilling("user-3")
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSetUserBillingStatus_Upsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	grace := time.Now().Add(48 * time.Hour)
	mock.ExpectExec(regexp.QuoteMeta(`
		INSERT INTO user_billing (user_id, status, grace_until, suspended_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW())`)).
		WithArgs("user-1", "past_due", grace, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.SetUserBillingStatus("user-1", "past_due", &grace, nil); err != nil {
		t.Fatalf("SetUserBillingStatus: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSetUserBillingStatus_ExecError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	boom := errors.New("write failed")
	mock.ExpectExec(regexp.QuoteMeta(`
		INSERT INTO user_billing (user_id, status, grace_until, suspended_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW())`)).
		WithArgs("user-1", "suspended", nil, nil, sqlmock.AnyArg()).
		WillReturnError(boom)

	err = s.SetUserBillingStatus("user-1", "suspended", nil, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListUserBillingByStatus_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"user_id", "status", "grace_until", "suspended_at", "grace_period",
		"r2_retention", "node_retention", "r2_quota_gb", "max_nodes", "max_links",
		"traffic_edge_gb", "traffic_relay_gb", "traffic_combined_gb",
		"manual_entitlement", "manual_entitlement_expires_at",
		"manual_entitlement_byon_expires_at", "manual_entitlement_route_expires_at",
		"manual_entitlement_granted_at", "manual_entitlement_granted_by",
		"overlimit_since", "traffic_ceiling_gb", "traffic_billing_enabled", "backup_billing_enabled", "admin_hold", "held_status", "node_links_off", "updated_at",
	}).
		AddRow("user-1", "suspended", nil, now, "", "", "", nil, nil, nil, nil, nil, nil, "", nil, nil, nil, nil, nil, nil, int64(0), false, false, false, "", false, now).
		AddRow("user-2", "suspended", nil, now, "", "", "", nil, nil, nil, nil, nil, nil, "", nil, nil, nil, nil, nil, nil, int64(0), false, false, false, "", false, now)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + userBillingCols + ` FROM user_billing WHERE status = $1`)).
		WithArgs("suspended").
		WillReturnRows(rows)

	out, err := s.ListUserBillingByStatus("suspended")
	if err != nil {
		t.Fatalf("ListUserBillingByStatus: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(out))
	}
	if out[0].UserID != "user-1" || out[0].SuspendedAt == nil {
		t.Fatalf("unexpected row 0: %+v", out[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListUserBillingByStatus_ScanErrorPropagates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	// A row with a wrong column count / type breaks the Scan for the second
	// row; ListUserBillingByStatus returns the error immediately (unlike the
	// tickets list which just `continue`s on a bad row).
	rows := sqlmock.NewRows([]string{
		"user_id", "status", "grace_until", "suspended_at", "grace_period",
		"r2_retention", "node_retention", "r2_quota_gb", "max_nodes", "max_links",
		"traffic_edge_gb", "traffic_relay_gb", "traffic_combined_gb",
		"manual_entitlement", "manual_entitlement_expires_at",
		"manual_entitlement_byon_expires_at", "manual_entitlement_route_expires_at",
		"manual_entitlement_granted_at", "manual_entitlement_granted_by",
		"overlimit_since", "traffic_ceiling_gb", "traffic_billing_enabled", "backup_billing_enabled", "admin_hold", "held_status", "node_links_off", "updated_at",
	}).
		AddRow("user-1", "suspended", nil, nil, "", "", "", nil, nil, nil, nil, nil, nil, "", nil, nil, nil, nil, nil, nil, int64(0), false, false, false, "", false, "not-a-time")

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + userBillingCols + ` FROM user_billing WHERE status = $1`)).
		WithArgs("suspended").
		WillReturnRows(rows)

	_, err = s.ListUserBillingByStatus("suspended")
	if err == nil {
		t.Fatalf("expected a scan error, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
