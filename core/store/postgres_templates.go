package store

import (
	"database/sql"
	"errors"
	"fmt"

	"dylaris-core/models"
)

// templateSelect reads templates joined to their owner for OwnerName; the owner
// always exists (owner_id cascades on user delete).
const templateSelect = `SELECT t.id, t.owner_id, t.name, t.kind, t.visibility, t.software, t.mc_version, t.software_version,
	t.storage_connection_id, t.size_bytes, t.created_at, t.updated_at, u.username
	FROM templates t JOIN users u ON u.id = t.owner_id`

func scanTemplate(sc interface{ Scan(...any) error }) (models.Template, error) {
	var t models.Template
	err := sc.Scan(&t.ID, &t.OwnerID, &t.Name, &t.Kind, &t.Visibility, &t.Software, &t.McVersion,
		&t.SoftwareVersion, &t.StorageConnectionID, &t.SizeBytes, &t.CreatedAt, &t.UpdatedAt, &t.OwnerName)
	return t, err
}

// CreateTemplate inserts t and fills in ID, Visibility and the timestamps.
func (s *PostgresStore) CreateTemplate(t *models.Template) error {
	if t.Visibility == "" {
		t.Visibility = "personal"
	}
	err := s.db.QueryRow(`INSERT INTO templates (owner_id, name, kind, visibility, software, mc_version, software_version, storage_connection_id, size_bytes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id, created_at, updated_at`,
		t.OwnerID, t.Name, t.Kind, t.Visibility, t.Software, t.McVersion, t.SoftwareVersion, t.StorageConnectionID, t.SizeBytes).
		Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("templates: create: %w", err)
	}
	return nil
}

// GetTemplate returns nil, nil when the template does not exist.
func (s *PostgresStore) GetTemplate(id int) (*models.Template, error) {
	t, err := scanTemplate(s.db.QueryRow(templateSelect+` WHERE t.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("templates: get %d: %w", id, err)
	}
	return &t, nil
}

// UpdateTemplate writes the editable fields. Kind, owner and storage stay as
// created: the stored bytes live on one backend and match one kind.
func (s *PostgresStore) UpdateTemplate(t *models.Template) error {
	if _, err := s.db.Exec(`UPDATE templates SET name = $2, visibility = $3, software = $4, mc_version = $5,
		software_version = $6, updated_at = NOW() WHERE id = $1`,
		t.ID, t.Name, t.Visibility, t.Software, t.McVersion, t.SoftwareVersion); err != nil {
		return fmt.Errorf("templates: update %d: %w", t.ID, err)
	}
	return nil
}

func (s *PostgresStore) DeleteTemplate(id int) error {
	if _, err := s.db.Exec(`DELETE FROM templates WHERE id = $1`, id); err != nil {
		return fmt.Errorf("templates: delete %d: %w", id, err)
	}
	return nil
}

// ListTemplatesVisibleTo returns the user's own templates, those shared with
// them (per template or owner-wide), plus every admin-level one when
// includeAdmin and every platform one when includePlatform.
func (s *PostgresStore) ListTemplatesVisibleTo(userID string, includeAdmin, includePlatform bool) ([]models.Template, error) {
	rows, err := s.db.Query(templateSelect+` WHERE t.owner_id = $1
		OR EXISTS (SELECT 1 FROM template_shares s WHERE s.template_id = t.id AND s.user_id = $1)
		OR EXISTS (SELECT 1 FROM template_owner_shares o WHERE o.owner_id = t.owner_id AND o.user_id = $1)
		OR (t.visibility = 'admin' AND $2::boolean)
		OR (t.visibility = 'platform' AND $3::boolean)
		ORDER BY t.name, t.id`, userID, includeAdmin, includePlatform)
	if err != nil {
		return nil, fmt.Errorf("templates: list for %s: %w", userID, err)
	}
	defer rows.Close()
	var out []models.Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, fmt.Errorf("templates: scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CountTemplatesByOwner(ownerID string) (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM templates WHERE owner_id = $1`, ownerID).Scan(&n); err != nil {
		return 0, fmt.Errorf("templates: count for %s: %w", ownerID, err)
	}
	return n, nil
}

func (s *PostgresStore) CountTemplatesOnStorageConnection(connID int) (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM templates WHERE storage_connection_id = $1`, connID).Scan(&n); err != nil {
		return 0, fmt.Errorf("templates: count on storage connection %d: %w", connID, err)
	}
	return n, nil
}

func (s *PostgresStore) SetTemplateSize(id int, size int64) error {
	if _, err := s.db.Exec(`UPDATE templates SET size_bytes = $2, updated_at = NOW() WHERE id = $1`, id, size); err != nil {
		return fmt.Errorf("templates: set size %d: %w", id, err)
	}
	return nil
}

func (s *PostgresStore) ListTemplateIDsByOwner(ownerID string) ([]int, error) {
	rows, err := s.db.Query(`SELECT id FROM templates WHERE owner_id = $1 ORDER BY id`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("templates: ids for %s: %w", ownerID, err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("templates: scan id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListTemplateShares(id int) ([]models.TemplateShare, error) {
	rows, err := s.db.Query(`SELECT s.template_id, s.user_id, u.username, s.level, s.created_at
		FROM template_shares s JOIN users u ON u.id = s.user_id WHERE s.template_id = $1 ORDER BY u.username`, id)
	if err != nil {
		return nil, fmt.Errorf("templates: shares of %d: %w", id, err)
	}
	return scanTemplateShares(rows)
}

// ListOwnerShares lists the owner-wide shares; TemplateID is 0 on those.
func (s *PostgresStore) ListOwnerShares(ownerID string) ([]models.TemplateShare, error) {
	rows, err := s.db.Query(`SELECT 0, s.user_id, u.username, s.level, s.created_at
		FROM template_owner_shares s JOIN users u ON u.id = s.user_id WHERE s.owner_id = $1 ORDER BY u.username`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("templates: owner shares of %s: %w", ownerID, err)
	}
	return scanTemplateShares(rows)
}

func scanTemplateShares(rows *sql.Rows) ([]models.TemplateShare, error) {
	defer rows.Close()
	var out []models.TemplateShare
	for rows.Next() {
		var sh models.TemplateShare
		if err := rows.Scan(&sh.TemplateID, &sh.UserID, &sh.Username, &sh.Level, &sh.CreatedAt); err != nil {
			return nil, fmt.Errorf("templates: scan share: %w", err)
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpsertTemplateShare(id int, userID, level string) error {
	if _, err := s.db.Exec(`INSERT INTO template_shares (template_id, user_id, level) VALUES ($1, $2, $3)
		ON CONFLICT (template_id, user_id) DO UPDATE SET level = EXCLUDED.level`, id, userID, level); err != nil {
		return fmt.Errorf("templates: share %d with %s: %w", id, userID, err)
	}
	return nil
}

func (s *PostgresStore) DeleteTemplateShare(id int, userID string) error {
	if _, err := s.db.Exec(`DELETE FROM template_shares WHERE template_id = $1 AND user_id = $2`, id, userID); err != nil {
		return fmt.Errorf("templates: unshare %d from %s: %w", id, userID, err)
	}
	return nil
}

func (s *PostgresStore) UpsertOwnerShare(ownerID, userID, level string) error {
	if _, err := s.db.Exec(`INSERT INTO template_owner_shares (owner_id, user_id, level) VALUES ($1, $2, $3)
		ON CONFLICT (owner_id, user_id) DO UPDATE SET level = EXCLUDED.level`, ownerID, userID, level); err != nil {
		return fmt.Errorf("templates: owner share %s with %s: %w", ownerID, userID, err)
	}
	return nil
}

func (s *PostgresStore) DeleteOwnerShare(ownerID, userID string) error {
	if _, err := s.db.Exec(`DELETE FROM template_owner_shares WHERE owner_id = $1 AND user_id = $2`, ownerID, userID); err != nil {
		return fmt.Errorf("templates: owner unshare %s from %s: %w", ownerID, userID, err)
	}
	return nil
}

// TemplateShareLevel returns the user's share level on a template, "" when
// there is none. A per-template and an owner-wide share can both apply; the
// higher one wins (edit over use).
func (s *PostgresStore) TemplateShareLevel(templateID int, userID string) (string, error) {
	var level sql.NullString
	err := s.db.QueryRow(`SELECT CASE WHEN COUNT(*) FILTER (WHERE level = 'edit') > 0 THEN 'edit'
			WHEN COUNT(*) > 0 THEN 'use' END
		FROM (
			SELECT level FROM template_shares WHERE template_id = $1 AND user_id = $2
			UNION ALL
			SELECT o.level FROM template_owner_shares o JOIN templates t ON t.owner_id = o.owner_id
				WHERE t.id = $1 AND o.user_id = $2
		) x`, templateID, userID).Scan(&level)
	if err != nil {
		return "", fmt.Errorf("templates: share level %d/%s: %w", templateID, userID, err)
	}
	return level.String, nil
}
