package store

import (
	"database/sql"

	"dylaris-core/models"
)

const shareLinkCols = `id, build_id, kind, token, expires_at, created_by, created_at, revoked`

func scanShareLink(row interface {
	Scan(dest ...interface{}) error
}) (*models.ShareLink, error) {
	var l models.ShareLink
	var exp sql.NullTime
	if err := row.Scan(&l.ID, &l.BuildID, &l.Kind, &l.Token, &exp,
		&l.CreatedBy, &l.CreatedAt, &l.Revoked); err != nil {
		return nil, err
	}
	if exp.Valid {
		t := exp.Time
		l.ExpiresAt = &t
	}
	return &l, nil
}

func (s *PostgresStore) CreateShareLink(l *models.ShareLink) (int, error) {
	var expiresAt interface{}
	if l.ExpiresAt != nil {
		expiresAt = *l.ExpiresAt
	}
	var id int
	err := s.db.QueryRow(`INSERT INTO share_links
		(build_id, kind, token, expires_at, created_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		l.BuildID, l.Kind, l.Token, expiresAt, l.CreatedBy,
	).Scan(&id)
	return id, err
}

// GetShareLinkByToken returns nil,nil when no row matches so the caller
// can 404 uniformly.
func (s *PostgresStore) GetShareLinkByToken(token string) (*models.ShareLink, error) {
	row := s.db.QueryRow(`SELECT `+shareLinkCols+`
		FROM share_links WHERE token=$1`, token)
	l, err := scanShareLink(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return l, err
}

func (s *PostgresStore) ListShareLinksByBuild(buildID int) ([]models.ShareLink, error) {
	rows, err := s.db.Query(`SELECT `+shareLinkCols+`
		FROM share_links WHERE build_id=$1 ORDER BY created_at DESC`, buildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.ShareLink{}
	for rows.Next() {
		l, err := scanShareLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// RevokeShareLink is owner-scoped (created_by): a user can only revoke their own
// links. Stamps revoked=TRUE instead of deleting so the row survives for audit.
func (s *PostgresStore) RevokeShareLink(id int, createdBy string) error {
	res, err := s.db.Exec(`UPDATE share_links SET revoked=TRUE
		WHERE id=$1 AND created_by=$2 AND revoked=FALSE`, id, createdBy)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
