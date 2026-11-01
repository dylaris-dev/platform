package database

import (
	"database/sql"
	"fmt"
)

// applyNodeExtraPortsHost adds the address players use for a node's extra
// ports (the voice preset's voice_host). NULL = the node's public IP, as
// before; set when that IP is not what players reach, for example behind NAT
// or a DNS name.
func applyNodeExtraPortsHost(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS extra_ports_host TEXT`); err != nil {
		return fmt.Errorf("node extra ports host schema: %w", err)
	}
	return nil
}
