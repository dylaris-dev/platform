package store

import (
	"database/sql"
	"errors"

	"dylaris-core/models"
)

// Operator overrides of the outgoing mail. A missing row is not an error, it is
// "this template has never been edited" - see database.createMailTemplateTables.

func (s *PostgresStore) ListMailTemplates() ([]models.MailTemplate, error) {
	rows, err := s.db.Query(`SELECT key, subject, body, updated_at FROM mail_templates ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.MailTemplate{}
	for rows.Next() {
		var t models.MailTemplate
		if err := rows.Scan(&t.Key, &t.Subject, &t.Body, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetMailTemplate returns nil, nil when the template has never been edited.
func (s *PostgresStore) GetMailTemplate(key string) (*models.MailTemplate, error) {
	var t models.MailTemplate
	err := s.db.QueryRow(
		`SELECT key, subject, body, updated_at FROM mail_templates WHERE key = $1`, key,
	).Scan(&t.Key, &t.Subject, &t.Body, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *PostgresStore) UpsertMailTemplate(t *models.MailTemplate) error {
	if t == nil || t.Key == "" {
		return errors.New("mail template: empty key")
	}
	_, err := s.db.Exec(`
		INSERT INTO mail_templates (key, subject, body, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (key) DO UPDATE
		   SET subject = EXCLUDED.subject, body = EXCLUDED.body, updated_at = NOW()`,
		t.Key, t.Subject, t.Body)
	return err
}

// DeleteMailTemplate is "reset to default": the row goes, and the built-in
// wording applies again from the next send.
func (s *PostgresStore) DeleteMailTemplate(key string) error {
	_, err := s.db.Exec(`DELETE FROM mail_templates WHERE key = $1`, key)
	return err
}
