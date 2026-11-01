package database

import (
	"database/sql"
	"fmt"
)

// applyTemplateModrinth adds the Modrinth references a template keeps instead
// of a stored copy of the jar: path is where the node lays the file, the rest
// is what it needs to download and verify it. One reference per path.
func applyTemplateModrinth(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS template_modrinth (
		id          SERIAL PRIMARY KEY,
		template_id INTEGER NOT NULL REFERENCES templates(id) ON DELETE CASCADE,
		path        TEXT NOT NULL,
		project_id  TEXT NOT NULL,
		version_id  TEXT NOT NULL,
		file_name   TEXT NOT NULL,
		url         TEXT NOT NULL,
		sha1        TEXT NOT NULL,
		sha512      TEXT NOT NULL,
		size_bytes  BIGINT NOT NULL,
		UNIQUE (template_id, path)
	)`); err != nil {
		return fmt.Errorf("template modrinth schema: %w", err)
	}
	return nil
}
