package store

import "time"

// RevokeSession records a signed-out session until expiresAt and drops the
// rows nothing can use any more.
func (s *PostgresStore) RevokeSession(id string, expiresAt time.Time) error {
	if _, err := s.db.Exec(`INSERT INTO revoked_sessions (id, expires_at) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET expires_at = GREATEST(revoked_sessions.expires_at, EXCLUDED.expires_at)`, id, expiresAt); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM revoked_sessions WHERE expires_at < NOW()`)
	return err
}

// IsSessionRevoked reports whether id is on the durable sign-out list.
func (s *PostgresStore) IsSessionRevoked(id string) (bool, error) {
	var ok bool
	err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM revoked_sessions WHERE id = $1 AND expires_at > NOW())`, id).Scan(&ok)
	return ok, err
}
