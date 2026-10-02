package store

import (
	"dylaris-core/models"
)

const sshKeyCols = `id, user_id, name, public_key, fingerprint, created_at`

// ListSSHKeysByUser returns an account's SFTP keys, newest first.
func (s *PostgresStore) ListSSHKeysByUser(userID string) ([]models.SSHKey, error) {
	rows, err := s.db.Query(`SELECT `+sshKeyCols+` FROM user_ssh_keys WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.SSHKey{}
	for rows.Next() {
		var k models.SSHKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.PublicKey, &k.Fingerprint, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListAllSSHKeys returns every account's keys, keyed by user id, for the SFTP
// sync's per-tick publish.
func (s *PostgresStore) ListAllSSHKeys() (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT user_id, public_key FROM user_ssh_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var uid, key string
		if err := rows.Scan(&uid, &key); err != nil {
			return nil, err
		}
		out[uid] = append(out[uid], key)
	}
	return out, rows.Err()
}

// AddSSHKey stores a key unless the account already holds maxPerUser keys or
// this one. added is false when the cap was reached; ErrSSHKeyExists when the
// account already has the key.
//
// ponytail: two adds at the same instant can each see room and land one over
// the cap. The cap keeps the per-tick publish small; it is not a security
// bound, so a row lock was not worth it.
func (s *PostgresStore) AddSSHKey(k *models.SSHKey, maxPerUser int) (added bool, err error) {
	res, err := s.db.Exec(`INSERT INTO user_ssh_keys (user_id, name, public_key, fingerprint)
		SELECT $1, $2, $3, $4
		WHERE (SELECT COUNT(*) FROM user_ssh_keys WHERE user_id = $1) < $5
		ON CONFLICT (user_id, fingerprint) DO NOTHING`,
		k.UserID, k.Name, k.PublicKey, k.Fingerprint, maxPerUser)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		return true, nil
	}
	var exists bool
	if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM user_ssh_keys WHERE user_id = $1 AND fingerprint = $2)`,
		k.UserID, k.Fingerprint).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, ErrSSHKeyExists
	}
	return false, nil
}

// DeleteSSHKey removes one of the account's keys. found is false when the
// account holds no key with that id.
func (s *PostgresStore) DeleteSSHKey(userID string, id int) (found bool, err error) {
	res, err := s.db.Exec(`DELETE FROM user_ssh_keys WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeleteAllSSHKeys removes every key of an account and returns how many.
func (s *PostgresStore) DeleteAllSSHKeys(userID string) (int, error) {
	res, err := s.db.Exec(`DELETE FROM user_ssh_keys WHERE user_id = $1`, userID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
