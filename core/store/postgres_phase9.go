package store

import (
	"context"
	"database/sql"
	"dylaris-core/models"
	"encoding/json"
	"errors"
	"time"
)

// RCON config + API keys. Kept separate from main postgres.go so
// the giant scan lists don't grow on every feature.

var errAPIKeyNotFound = errors.New("api key not found")

// ErrAPIKeyRollConflict: the key was rolled, revoked or expired since the
// caller read it (or is not the caller's).
var ErrAPIKeyRollConflict = errors.New("api key changed while rolling")

// --- RCON config ---

func (s *PostgresStore) GetServerRconConfig(serverID int) (bool, int, string, error) {
	var enabled bool
	var port int
	var password string
	err := s.db.QueryRow(`SELECT rcon_enabled, rcon_port, rcon_password
		FROM servers WHERE id=$1`, serverID).Scan(&enabled, &port, &password)
	return enabled, port, password, err
}

func (s *PostgresStore) SetServerRconConfig(serverID int, enabled bool, port int, password string) error {
	_, err := s.db.Exec(`UPDATE servers SET rcon_enabled=$2, rcon_port=$3, rcon_password=$4
		WHERE id=$1`, serverID, enabled, port, password)
	return err
}

func (s *PostgresStore) GetServerRconNeedsRestart(serverID int) (bool, error) {
	var needs bool
	err := s.db.QueryRow(`SELECT rcon_needs_restart FROM servers WHERE id=$1`, serverID).Scan(&needs)
	return needs, err
}

func (s *PostgresStore) SetServerRconNeedsRestart(serverID int, needsRestart bool) error {
	_, err := s.db.Exec(`UPDATE servers SET rcon_needs_restart=$2 WHERE id=$1`, serverID, needsRestart)
	return err
}

// --- API keys ---

const apiKeyCols = `id, user_id, name, key_hash, scope, last_used_at, expires_at,
		revoked_at, rate_per_min, created_at`

func scanAPIKey(row interface {
	Scan(dest ...interface{}) error
}) (*models.APIKey, error) {
	var k models.APIKey
	var scopeJSON []byte
	var last, exp, rev sql.NullTime
	if err := row.Scan(&k.ID, &k.UserID, &k.Name, &k.KeyHash, &scopeJSON,
		&last, &exp, &rev, &k.RatePerMin, &k.CreatedAt); err != nil {
		return nil, err
	}
	if len(scopeJSON) > 0 {
		_ = json.Unmarshal(scopeJSON, &k.Scope)
	}
	if last.Valid {
		t := last.Time
		k.LastUsedAt = &t
	}
	if exp.Valid {
		t := exp.Time
		k.ExpiresAt = &t
	}
	if rev.Valid {
		t := rev.Time
		k.RevokedAt = &t
	}
	return &k, nil
}

func (s *PostgresStore) CreateAPIKey(k *models.APIKey) (int, error) {
	scopeJSON, err := json.Marshal(k.Scope)
	if err != nil {
		return 0, err
	}
	rate := k.RatePerMin
	if rate <= 0 {
		rate = 60
	}
	var expiresAt interface{}
	if k.ExpiresAt != nil {
		expiresAt = *k.ExpiresAt
	}
	var id int
	err = s.db.QueryRow(`INSERT INTO api_keys
		(user_id, name, key_hash, scope, expires_at, rate_per_min)
		VALUES ($1,$2,$3,$4::jsonb,$5,$6) RETURNING id`,
		k.UserID, k.Name, k.KeyHash, string(scopeJSON), expiresAt, rate,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) ListAPIKeysByUser(userID string) ([]models.APIKey, error) {
	rows, err := s.db.Query(`SELECT `+apiKeyCols+`
		FROM api_keys WHERE user_id=$1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// GetAPIKeyByHash also matches the previous hash of a rolled key, but only
// while the grace runs and the key itself is neither revoked nor expired: a
// dead key's old hash is dead too. ViaPrevious tells the caller which one it
// presented.
func (s *PostgresStore) GetAPIKeyByHash(hash string) (*models.APIKey, error) {
	row := s.db.QueryRow(`SELECT `+apiKeyCols+`
		FROM api_keys WHERE key_hash=$1
		OR (prev_key_hash=$1 AND prev_key_expires_at > NOW()
			AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW()))`, hash)
	k, err := scanAPIKey(row)
	if err == sql.ErrNoRows {
		return nil, errAPIKeyNotFound
	}
	if err == nil && k.KeyHash != hash {
		k.ViaPrevious = true
	}
	return k, err
}

// RollAPIKey replaces the key's hash and keeps the old one valid for grace.
// The two hashes move in one statement, so a reader never sees a key with no
// valid hash; a second roll overwrites the previous hash (one old key only).
func (s *PostgresStore) RollAPIKey(ctx context.Context, id int, ownerID, expectHash, newHash string, grace time.Duration) error {
	res, err := s.db.ExecContext(ctx, `UPDATE api_keys
		SET prev_key_hash=key_hash, prev_key_expires_at=NOW() + make_interval(secs => $3),
			key_hash=$2
		WHERE id=$1 AND user_id=$4 AND key_hash=$5
			AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())`,
		id, newHash, grace.Seconds(), ownerID, expectHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAPIKeyRollConflict
	}
	return nil
}

// RevokeAPIKey is user-scoped — owners can only revoke their own keys.
// Stamps revoked_at instead of deleting so audit/last-used data survives.
func (s *PostgresStore) RevokeAPIKey(id int, userID string) error {
	res, err := s.db.Exec(`UPDATE api_keys SET revoked_at=NOW()
		WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errAPIKeyNotFound
	}
	return nil
}

func (s *PostgresStore) TouchAPIKey(id int) error {
	_, err := s.db.Exec(`UPDATE api_keys SET last_used_at=NOW() WHERE id=$1`, id)
	return err
}
