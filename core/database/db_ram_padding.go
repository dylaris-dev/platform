package database

import (
	"database/sql"
	"fmt"
)

// applyRAMPaddingSchema adds the per-node and per-server container RAM
// padding overrides. NULL inherits the next level (server -> node -> the
// placement.ram_padding_mb setting -> 512); 0 is a real "no padding".
func applyRAMPaddingSchema(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS ram_padding_mb INTEGER`); err != nil {
		return fmt.Errorf("nodes: add ram_padding_mb: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE servers ADD COLUMN IF NOT EXISTS ram_padding_mb INTEGER`); err != nil {
		return fmt.Errorf("servers: add ram_padding_mb: %w", err)
	}
	return nil
}
