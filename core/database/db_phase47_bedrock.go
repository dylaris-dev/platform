package database

import (
	"database/sql"
	"fmt"
)

// applyBedrockSchema adds the per-server Floodgate key table. A row means
// Bedrock joins are enabled for that server; the key is sealed at rest
// (store.bedrockFloodgatePurpose) and dies with the server.
func applyBedrockSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS bedrock_servers (
		server_id         INTEGER PRIMARY KEY REFERENCES servers(id) ON DELETE CASCADE,
		floodgate_key_enc TEXT NOT NULL,
		enabled_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("bedrock schema: %w", err)
	}
	// Per-server Bedrock traffic (observability, like voice_usage), and the
	// Bedrock SUBSET of traffic_usage_region.bytes. The bill is unchanged:
	// Bedrock bytes are already inside the edge totals.
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS bedrock_usage (
			server_id  INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
			period     DATE NOT NULL,
			region     TEXT NOT NULL,
			rx_bytes   BIGINT NOT NULL DEFAULT 0,
			tx_bytes   BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (server_id, period, region)
		)`,
		`ALTER TABLE traffic_usage_region ADD COLUMN IF NOT EXISTS bedrock_bytes BIGINT NOT NULL DEFAULT 0`,
		// Which editions a route takes. No row = Java only (every route that
		// predates this); deleted with the route by the panel, with the server
		// by the cascade.
		`CREATE TABLE IF NOT EXISTS route_editions (
			domain     TEXT PRIMARY KEY,
			server_id  INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
			java       BOOLEAN NOT NULL,
			bedrock    BOOLEAN NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("bedrock usage schema: %w", err)
		}
	}
	return nil
}
