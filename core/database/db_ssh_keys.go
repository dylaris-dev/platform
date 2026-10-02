package database

import (
	"database/sql"
	"fmt"
)

// applySSHKeysSchema creates the table of SSH public keys an account signs in
// to SFTP with.
//
// An account with a second factor cannot sign in to SFTP with its password:
// SFTP has nowhere to ask for the code, so the password alone opened every file
// the account could reach, which is the case the second factor exists for. A
// key added from a panel session that passed that factor is what it uses
// instead. Keys are stored as "<type> <base64>", without the comment, and are
// unique per account by fingerprint.
func applySSHKeysSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS user_ssh_keys (
		id           SERIAL PRIMARY KEY,
		user_id      UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name         VARCHAR(128) NOT NULL,
		public_key   TEXT         NOT NULL,
		fingerprint  VARCHAR(128) NOT NULL,
		created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
		UNIQUE (user_id, fingerprint)
	)`); err != nil {
		return fmt.Errorf("ssh keys: create user_ssh_keys: %w", err)
	}
	return nil
}
