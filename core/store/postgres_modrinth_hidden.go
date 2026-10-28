package store

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
