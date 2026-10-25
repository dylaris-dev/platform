package database

import (
	"database/sql"
	"fmt"
)

// applyServerPortsSchema adds extra ports per server for direct routing: a
// self-hosted node publishes them next to the game port (voice chat, a map's
// web view). The unique key is per node and protocol because that is what a
// host port collides on. node_id is the node the port was taken on; a server
// that moves gets its ports re-taken on the new node at the next start.
//
// port_permissions answers "may this server have extra ports" by scope
// (server:<id> > user:<id> > role:<id> > the setting ports.extra_enabled);
// port_limits caps how many a server owner may hold (user:<id> > role:<id> >
// the setting ports.max_per_user). A missing limit row defers to the next
// scope, a row with NULL means no limit.
func applyServerPortsSchema(db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS server_ports (
			id             SERIAL PRIMARY KEY,
			server_id      INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
			node_id        INTEGER NOT NULL,
			protocol       TEXT NOT NULL CHECK (protocol IN ('tcp', 'udp')),
			host_port      INTEGER NOT NULL CHECK (host_port BETWEEN 1024 AND 65535),
			container_port INTEGER NOT NULL CHECK (container_port BETWEEN 1 AND 65535),
			name           TEXT NOT NULL DEFAULT '',
			note           TEXT NOT NULL DEFAULT '',
			preset         TEXT CHECK (preset IN ('svc', 'plasmo')),
			created_by     UUID REFERENCES users(id) ON DELETE SET NULL,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (node_id, protocol, host_port)
		)`,
		`CREATE INDEX IF NOT EXISTS server_ports_server ON server_ports(server_id)`,
		`CREATE TABLE IF NOT EXISTS port_permissions (
			scope      TEXT PRIMARY KEY,
			allowed    BOOLEAN NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS port_limits (
			scope      TEXT PRIMARY KEY,
			max_ports  INTEGER CHECK (max_ports >= 0),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS extra_port_range TEXT`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("server ports schema: %w", err)
		}
	}
	return nil
}
