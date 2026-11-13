package store

import (
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestGetDefaultPlan_Found(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"id", "name", "price_label", "max_nodes", "max_links", "r2_quota_gb",
		"traffic_edge_gb", "traffic_relay_gb", "traffic_combined_gb", "is_default", "kind", "created_at",
	}).AddRow(1, "Starter", "$5/mo", int64(2), int64(1), int64(10), int64(50), int64(50), int64(100), true, "both", now)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + planCols + ` FROM plans WHERE is_default LIMIT 1`)).
		WillReturnRows(rows)

	p, err := s.GetDefaultPlan()
	if err != nil {
		t.Fatalf("GetDefaultPlan: %v", err)
	}
	if p == nil || p.Name != "Starter" || !p.IsDefault {
		t.Fatalf("unexpected plan: %+v", p)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestGetDefaultPlan_NoneSet pins "no default plan configured" as (nil, nil)
// — not an error — so callers can treat it as "unlimited" per the doc comment.
func TestGetDefaultPlan_NoneSet(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + planCols + ` FROM plans WHERE is_default LIMIT 1`)).
		WillReturnError(sql.ErrNoRows)

	p, err := s.GetDefaultPlan()
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if p != nil {
		t.Fatalf("expected nil plan, got %+v", p)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCreatePlan_ClearsExistingDefault pins the single-default invariant: when
// the new plan is default, CreatePlan first clears any existing default
// inside the same tx before inserting, then commits.
func TestCreatePlan_ClearsExistingDefault(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	p := Plan{
		Name: "Pro", PriceLabel: "$20/mo", MaxNodes: 10, MaxLinks: 5,
		R2QuotaGB: 200, TrafficEdgeGB: 500, TrafficRelayGB: 500, TrafficCombinedGB: 1000,
		IsDefault: true,
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE plans SET is_default = FALSE WHERE is_default`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`
		INSERT INTO plans (name, price_label, max_nodes, max_links, r2_quota_gb, traffic_edge_gb, traffic_relay_gb, traffic_combined_gb, is_default, kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`)).
		WithArgs(p.Name, p.PriceLabel, p.MaxNodes, p.MaxLinks, p.R2QuotaGB,
			p.TrafficEdgeGB, p.TrafficRelayGB, p.TrafficCombinedGB, p.IsDefault, "both").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(5))
	mock.ExpectCommit()

	id, err := s.CreatePlan(p)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if id != 5 {
		t.Fatalf("got id %d, want 5", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCreatePlan_NotDefault pins the other branch: a non-default plan skips
// the clear-existing-default step entirely.
func TestCreatePlan_NotDefault(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	p := Plan{Name: "Basic", PriceLabel: "$0/mo", IsDefault: false}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`
		INSERT INTO plans (name, price_label, max_nodes, max_links, r2_quota_gb, traffic_edge_gb, traffic_relay_gb, traffic_combined_gb, is_default, kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`)).
		WithArgs(p.Name, p.PriceLabel, p.MaxNodes, p.MaxLinks, p.R2QuotaGB,
			p.TrafficEdgeGB, p.TrafficRelayGB, p.TrafficCombinedGB, p.IsDefault, "both").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(6))
	mock.ExpectCommit()

	id, err := s.CreatePlan(p)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if id != 6 {
		t.Fatalf("got id %d, want 6", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCreatePlan_InsertErrorRollsBack pins that an INSERT failure returns the
// error and the deferred tx.Rollback() fires (no Commit expected).
func TestCreatePlan_InsertErrorRollsBack(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	p := Plan{Name: "Basic", IsDefault: false}
	boom := errors.New("insert failed")

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`
		INSERT INTO plans (name, price_label, max_nodes, max_links, r2_quota_gb, traffic_edge_gb, traffic_relay_gb, traffic_combined_gb, is_default, kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`)).
		WithArgs(p.Name, p.PriceLabel, p.MaxNodes, p.MaxLinks, p.R2QuotaGB,
			p.TrafficEdgeGB, p.TrafficRelayGB, p.TrafficCombinedGB, p.IsDefault, "both").
		WillReturnError(boom)
	mock.ExpectRollback()

	_, err = s.CreatePlan(p)
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestUpdatePlan_ClearsOtherDefault pins UpdatePlan's variant of the
// single-default invariant: the clear-default UPDATE excludes the plan being
// updated (id <> $1) so a plan being re-saved as default doesn't clear itself.
func TestUpdatePlan_ClearsOtherDefault(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	p := Plan{
		ID: 3, Name: "Pro", PriceLabel: "$20/mo", MaxNodes: 10, MaxLinks: 5,
		R2QuotaGB: 200, TrafficEdgeGB: 500, TrafficRelayGB: 500, TrafficCombinedGB: 1000,
		IsDefault: true,
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE plans SET is_default = FALSE WHERE is_default AND id <> $1`)).
		WithArgs(p.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`
		UPDATE plans SET name=$2, price_label=$3, max_nodes=$4, max_links=$5, r2_quota_gb=$6,
			traffic_edge_gb=$7, traffic_relay_gb=$8, traffic_combined_gb=$9, is_default=$10, kind=$11
		WHERE id=$1`)).
		WithArgs(p.ID, p.Name, p.PriceLabel, p.MaxNodes, p.MaxLinks, p.R2QuotaGB,
			p.TrafficEdgeGB, p.TrafficRelayGB, p.TrafficCombinedGB, p.IsDefault, "both").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := s.UpdatePlan(p); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestUpdatePlan_NotDefaultSkipsClear pins that a non-default update skips
// the clear-default step and goes straight to the row UPDATE.
func TestUpdatePlan_NotDefaultSkipsClear(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	p := Plan{ID: 4, Name: "Basic", IsDefault: false}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`
		UPDATE plans SET name=$2, price_label=$3, max_nodes=$4, max_links=$5, r2_quota_gb=$6,
			traffic_edge_gb=$7, traffic_relay_gb=$8, traffic_combined_gb=$9, is_default=$10, kind=$11
		WHERE id=$1`)).
		WithArgs(p.ID, p.Name, p.PriceLabel, p.MaxNodes, p.MaxLinks, p.R2QuotaGB,
			p.TrafficEdgeGB, p.TrafficRelayGB, p.TrafficCombinedGB, p.IsDefault, "both").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := s.UpdatePlan(p); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestCountLinkKitsByOwner_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM warp_api_keys
		WHERE owner_id = $1::uuid AND revoked_at IS NULL AND retire_at IS NULL AND node_id LIKE 'link-%'`)).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	n, err := s.CountLinkKitsByOwner("user-1")
	if err != nil {
		t.Fatalf("CountLinkKitsByOwner: %v", err)
	}
	if n != 3 {
		t.Fatalf("got %d, want 3", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
