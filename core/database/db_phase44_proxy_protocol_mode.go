package database

import (
	"database/sql"
	"fmt"
)

// applyProxyProtocolModeSchema adds the owner's choice for real player IP
// forwarding, next to servers.proxy_protocol, which stays what it was: the
// forwarding actually applied (hub routes, node key).
//
//	auto  forward once the server shows it expects a PROXY header (the node's
//	      status ping answered with one); the default
//	on    always forward (the switch as it was before; the server must be set up)
//	off   never forward
//
// The one-time backfill keeps every server that had the switch on at "on", so
// nothing changes for them. It runs only in the boot that creates the column:
// afterwards proxy_protocol=true can mean "auto, and active", which must not be
// turned into "on".
func applyProxyProtocolModeSchema(db *sql.DB) error {
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'servers' AND column_name = 'proxy_protocol_mode')`).Scan(&exists); err != nil {
		return fmt.Errorf("proxy protocol mode schema: %w", err)
	}
	if exists {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("proxy protocol mode schema: %w", err)
	}
	defer tx.Rollback()
	for _, q := range []string{
		`ALTER TABLE servers ADD COLUMN IF NOT EXISTS proxy_protocol_mode TEXT NOT NULL DEFAULT 'auto'
			CHECK (proxy_protocol_mode IN ('auto', 'on', 'off'))`,
		`UPDATE servers SET proxy_protocol_mode = 'on' WHERE proxy_protocol`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("proxy protocol mode schema: %w", err)
		}
	}
	return tx.Commit()
}
