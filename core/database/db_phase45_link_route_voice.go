package database

import (
	"database/sql"
	"fmt"
)

// applyLinkRouteVoiceSchema adds voice chat for route-only addresses. A
// route-only entry has no server row, so it cannot hold a voice_ports row;
// this is the same port, keyed by the route's domain instead. The pool is
// shared with voice_ports per region: both allocators take the same advisory
// lock and skip the other table's ports, because UNIQUE (region, port) only
// holds within one table. target_port is the voice mod's port on the
// customer's machine, on the route's own target host.
func applyLinkRouteVoiceSchema(db *sql.DB) error {
	q := `CREATE TABLE IF NOT EXISTS link_route_voice (
		domain      TEXT PRIMARY KEY REFERENCES core_link_routes(domain) ON DELETE CASCADE,
		region      TEXT NOT NULL,
		port        INTEGER NOT NULL,
		target_port INTEGER NOT NULL CHECK (target_port BETWEEN 1 AND 65535),
		uplink_key  BYTEA NOT NULL,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE (region, port)
	)`
	if _, err := db.Exec(q); err != nil {
		return fmt.Errorf("link route voice schema: %w", err)
	}
	return nil
}
