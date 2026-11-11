package store

import (
	"fmt"

	"dylaris-core/models"
)

// CreateBeamIssuerKeyUnderCap inserts a key only while the owner holds fewer
// than cap live ones, counted and inserted under one per-owner lock so two
// registrations at the same instant cannot both see room.
func (s *PostgresStore) CreateBeamIssuerKeyUnderCap(k models.BeamIssuerKey, cap int) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('beam-issuer-cap:' || $1))`, k.OwnerID); err != nil {
		return false, fmt.Errorf("lock beam issuer keys of %s: %w", k.OwnerID, err)
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM beam_issuer_keys
		WHERE owner_id = $1::uuid AND revoked_at IS NULL`, k.OwnerID).Scan(&n); err != nil {
		return false, err
	}
	if n >= cap {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO beam_issuer_keys (kid, owner_id, public_key) VALUES ($1, $2::uuid, $3)`,
		k.ID, k.OwnerID, k.PublicKey); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListBeamIssuerKeysByOwner returns every key of the owner, revoked ones
// included (the publisher deletes their records), oldest first.
func (s *PostgresStore) ListBeamIssuerKeysByOwner(ownerID string) ([]models.BeamIssuerKey, error) {
	rows, err := s.db.Query(`SELECT kid, owner_id::text, public_key, created_at, revoked_at
		FROM beam_issuer_keys WHERE owner_id = $1::uuid ORDER BY created_at, kid`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.BeamIssuerKey{}
	for rows.Next() {
		var k models.BeamIssuerKey
		if err := rows.Scan(&k.ID, &k.OwnerID, &k.PublicKey, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeBeamIssuerKey marks one of the owner's live keys revoked. found is
// false when the owner holds no live key with that kid.
func (s *PostgresStore) RevokeBeamIssuerKey(ownerID, kid string) (bool, error) {
	res, err := s.db.Exec(`UPDATE beam_issuer_keys SET revoked_at = NOW()
		WHERE kid = $1 AND owner_id = $2::uuid AND revoked_at IS NULL`, kid, ownerID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ListBeamIssuerOwners returns every owner holding a key, revoked or not.
func (s *PostgresStore) ListBeamIssuerOwners() ([]string, error) {
	return s.beamIssuerColumn(`SELECT DISTINCT owner_id::text FROM beam_issuer_keys ORDER BY 1`)
}

// ListBeamIssuerKids returns every kid with a row, revoked or not.
func (s *PostgresStore) ListBeamIssuerKids() ([]string, error) {
	return s.beamIssuerColumn(`SELECT kid FROM beam_issuer_keys`)
}

func (s *PostgresStore) beamIssuerColumn(q string) ([]string, error) {
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
