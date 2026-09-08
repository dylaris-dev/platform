package database

import (
	"database/sql"
	"fmt"
)

// createMailTemplateTables holds an operator's overrides of the outgoing mail.
//
// One row per template that has been EDITED. No row means the built-in wording,
// which is why an install that never opens the screen keeps sending exactly
// what it sent before, and why "reset to default" is a DELETE: writing the
// original text back into the row would freeze today's wording and never pick
// up a later improvement to it.
//
// The key is the definition's key (mailer.KeyVerifyEmail and friends), not a
// serial. A template is identified by what it IS, and a row whose definition no
// longer exists after an upgrade is simply never read - it costs nothing and
// keeps an operator's text should the key come back.
func createMailTemplateTables(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS mail_templates (
			key        VARCHAR(64) PRIMARY KEY,
			subject    TEXT NOT NULL DEFAULT '',
			body       TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("mail templates: %w", err)
		}
	}
	return nil
}
