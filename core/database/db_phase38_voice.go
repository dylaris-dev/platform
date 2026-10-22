package database

import (
	"database/sql"
	"fmt"
)

// applyVoiceSchema adds the tables behind proximity voice chat.
//
// voice_ports holds one public UDP port per server, unique per REGION (each
// region's edges own their own pool), plus the uplink key the node uses toward
// the edge. host is stored with it so the reconcile and the start-time
// re-assert do not have to read the routes again.
//
// voice_usage is the per-server voice split for observability. Billing does not
// read it: voice bytes stay inside traffic_usage.edge_bytes and
// traffic_usage_region.bytes, and traffic_usage_region.voice_bytes is a SUBSET
// of bytes (kind edge only), never a new kind - limits resolve per
// (region, kind), and a voice kind would take voice out of the player pool.
func applyVoiceSchema(db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS voice_ports (
			server_id     INTEGER PRIMARY KEY REFERENCES servers(id) ON DELETE CASCADE,
			region        TEXT NOT NULL,
			port          INTEGER NOT NULL,
			mod           TEXT NOT NULL CHECK (mod IN ('svc','plasmo')),
			internal_port INTEGER NOT NULL,
			uplink_key    BYTEA NOT NULL,
			host          TEXT NOT NULL,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (region, port)
		)`,
		`CREATE TABLE IF NOT EXISTS voice_usage (
			server_id  INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
			period     DATE NOT NULL,
			region     TEXT NOT NULL,
			rx_bytes   BIGINT NOT NULL DEFAULT 0,
			tx_bytes   BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (server_id, period, region)
		)`,
		`ALTER TABLE traffic_usage_region ADD COLUMN IF NOT EXISTS voice_bytes BIGINT NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("voice schema: %s: %w", q, err)
		}
	}
	return nil
}
