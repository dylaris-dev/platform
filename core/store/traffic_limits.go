package store

import (
	"database/sql"
	"errors"

	"dylaris-core/models"
)

// Traffic limits, per scope and per (region, kind).
//
// Deliberately the same shape as GetGatewayRouteLimit and friends: one row per
// scope, scopes walked most-specific-first by the caller, and a row that exists
// has answered even when its values are NULL. Copying that shape is the point -
// there is one way limits work on this platform and this is it.

// GetTrafficLimit returns the row one scope holds for a (region, kind), or nil
// when that scope says nothing about it. nil is NOT "no limit": it means the
// caller should ask the next scope down.
func (s *PostgresStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	var l models.TrafficLimit
	err := s.db.QueryRow(`
		SELECT id, scope, region, kind, included_gb, max_purchase_gb
		FROM traffic_limits WHERE scope = $1 AND region = $2 AND kind = $3`,
		scope, region, kind,
	).Scan(&l.ID, &l.Scope, &l.Region, &l.Kind, &l.IncludedGB, &l.MaxPurchaseGB)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// SetTrafficLimit writes one scope's answer for a (region, kind). Passing nil
// for either value stores NULL, which is a decision - "set, and no limit" - not
// an absence. To make a scope say nothing again, delete the row.
func (s *PostgresStore) SetTrafficLimit(scope, region, kind string, includedGB, maxPurchaseGB *int64) error {
	_, err := s.db.Exec(`
		INSERT INTO traffic_limits (scope, region, kind, included_gb, max_purchase_gb)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (scope, region, kind) DO UPDATE SET
			included_gb     = EXCLUDED.included_gb,
			max_purchase_gb = EXCLUDED.max_purchase_gb`,
		scope, region, kind, includedGB, maxPurchaseGB)
	return err
}

// ListTrafficLimits returns every row, ordered so the panel can group them.
func (s *PostgresStore) ListTrafficLimits() ([]models.TrafficLimit, error) {
	rows, err := s.db.Query(`
		SELECT id, scope, region, kind, included_gb, max_purchase_gb
		FROM traffic_limits ORDER BY scope, region, kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.TrafficLimit
	for rows.Next() {
		var l models.TrafficLimit
		if err := rows.Scan(&l.ID, &l.Scope, &l.Region, &l.Kind, &l.IncludedGB, &l.MaxPurchaseGB); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteTrafficLimit removes one scope's answer, which is how a scope goes back
// to saying nothing. Deleting is NOT the same as storing NULL: this makes the
// next scope down answer, storing NULL answers "no limit" here and stops.
func (s *PostgresStore) DeleteTrafficLimit(scope, region, kind string) error {
	_, err := s.db.Exec(`DELETE FROM traffic_limits WHERE scope = $1 AND region = $2 AND kind = $3`,
		scope, region, kind)
	return err
}

// ReplaceTrafficLimits makes rows the exact set of the STORE-OWNED rows one
// scope holds: the ("*", edge) row, the only shape the store writes and the
// one shape an operator is refused (handlers/billing/traffic_limits.go). Every
// other row of the scope is an operator's override and is left alone; it used
// to be deleted on every managed sync. One transaction, so a failed write
// never leaves a tenant with half of what the store sent.
func (s *PostgresStore) ReplaceTrafficLimits(scope string, rows []models.TrafficLimit) error {
	for _, r := range rows {
		if r.Region != "*" || r.Kind != "edge" {
			return errors.New("only the (*, edge) traffic row is store-owned")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM traffic_limits WHERE scope = $1 AND region = '*' AND kind = 'edge'`, scope); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.Exec(`
			INSERT INTO traffic_limits (scope, region, kind, included_gb, max_purchase_gb)
			VALUES ($1,$2,$3,$4,$5)`,
			scope, r.Region, r.Kind, r.IncludedGB, r.MaxPurchaseGB); err != nil {
			return err
		}
	}
	return tx.Commit()
}
