package store

import (
	"database/sql"

	"dylaris-core/models"
)

// HiddenModrinthProjects lists the Modrinth projects a user hid from the
// import list. Hiding is only a view preference; nothing else reads it.
func (s *PostgresStore) HiddenModrinthProjects(userID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT project_id FROM modrinth_hidden_projects WHERE user_id=$1 ORDER BY project_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetModrinthProjectHidden hides or shows one project for one user.
func (s *PostgresStore) SetModrinthProjectHidden(userID, projectID string, hidden bool) error {
	if hidden {
		_, err := s.db.Exec(`INSERT INTO modrinth_hidden_projects(user_id, project_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, userID, projectID)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM modrinth_hidden_projects WHERE user_id=$1 AND project_id=$2`, userID, projectID)
	return err
}

// ListModrinthBoundPacks lists every pack bound to a Modrinth project, for
// the background sync of imported versions.
func (s *PostgresStore) ListModrinthBoundPacks() ([]models.Pack, error) {
	rows, err := s.db.Query(`SELECT ` + packCols + ` FROM packs WHERE modrinth_project_id <> '' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.Pack{}
	for rows.Next() {
		p, err := scanPack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetPackByModrinthProject is the one pack bound to a Modrinth project, or nil.
func (s *PostgresStore) GetPackByModrinthProject(projectID string) (*models.Pack, error) {
	p, err := scanPack(s.db.QueryRow(`SELECT `+packCols+` FROM packs WHERE modrinth_project_id=$1 AND modrinth_project_id <> ''`, projectID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// GetModrinthMembership is the cached membership, or nil when never checked.
func (s *PostgresStore) GetModrinthMembership(userID, projectID string) (*models.ModrinthMembership, error) {
	var m models.ModrinthMembership
	err := s.db.QueryRow(`SELECT user_id, project_id, permissions, state, checked_at, confirmed_at FROM modrinth_memberships WHERE user_id=$1 AND project_id=$2`, userID, projectID).
		Scan(&m.UserID, &m.ProjectID, &m.Permissions, &m.State, &m.CheckedAt, &m.ConfirmedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &m, err
}

// UpsertModrinthMembership stores the latest answer for a user and project.
// ConfirmedAt is written as given: the caller carries the last real
// confirmation forward, so a non-member answer cannot extend it.
func (s *PostgresStore) UpsertModrinthMembership(m *models.ModrinthMembership) error {
	_, err := s.db.Exec(`INSERT INTO modrinth_memberships(user_id, project_id, permissions, state, checked_at, confirmed_at) VALUES ($1,$2,$3,$4,NOW(),$5)
		ON CONFLICT (user_id, project_id) DO UPDATE SET permissions=EXCLUDED.permissions, state=EXCLUDED.state, checked_at=NOW(), confirmed_at=EXCLUDED.confirmed_at`,
		m.UserID, m.ProjectID, m.Permissions, m.State, m.ConfirmedAt)
	return err
}

// ListModrinthMemberships lists every cached membership of a user.
func (s *PostgresStore) ListModrinthMemberships(userID string) ([]models.ModrinthMembership, error) {
	rows, err := s.db.Query(`SELECT user_id, project_id, permissions, state, checked_at, confirmed_at FROM modrinth_memberships WHERE user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.ModrinthMembership{}
	for rows.Next() {
		var m models.ModrinthMembership
		if err := rows.Scan(&m.UserID, &m.ProjectID, &m.Permissions, &m.State, &m.CheckedAt, &m.ConfirmedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ClearModrinthMemberships forgets every membership of a user that is not a
// live "member" answer: packs the user lost (or cannot prove) access to then
// leave their list. Nothing is deleted from the packs themselves.
func (s *PostgresStore) ClearModrinthMemberships(userID string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM modrinth_memberships WHERE user_id=$1 AND state <> 'member'`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
