package database

import (
	"database/sql"
	"fmt"
)

// applyProxyProtocolSchema adds the per-route switch that makes the gateway
// hand a backend the real player address in a PROXY v2 header.
//
// servers.proxy_protocol is the switch for a managed server: Core carries it
// on create_route, sends set_server_proxy_protocol when it moves, and mirrors
// it to dylaris:server:<uuid>:proxy_protocol for the node's status ping.
// core_link_routes.proxy_protocol is the same switch for a route-only entry,
// which Core publishes into Redis itself.
//
// Off by default and additive: a server or route that never touches it
// behaves exactly as before, and the backend sees no header.
func applyProxyProtocolSchema(db *sql.DB) error {
	for _, q := range []string{
		`ALTER TABLE servers ADD COLUMN IF NOT EXISTS proxy_protocol BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE core_link_routes ADD COLUMN IF NOT EXISTS proxy_protocol BOOLEAN NOT NULL DEFAULT FALSE`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("proxy protocol: %s: %w", q, err)
		}
	}
	return nil
}
