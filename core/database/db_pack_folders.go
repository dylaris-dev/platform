package database

import (
	"database/sql"
	"fmt"
)

func applyPackFoldersSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS pack_build_folders(build_id INTEGER NOT NULL REFERENCES pack_builds(id) ON DELETE CASCADE,path TEXT NOT NULL,PRIMARY KEY(build_id,path))`)
	if err != nil {
		return fmt.Errorf("pack folders schema: %w", err)
	}
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS modversions_storage_key_idx ON modversions(storage_key)`); err != nil {
		return fmt.Errorf("pack upload references index: %w", err)
	}
	return nil
}
