package database

import (
	"database/sql"
	"fmt"
)

// applyTrafficSeen adds the table the traffic aggregator's "billed up to here"
// markers live in (managed gateway stage 5, M1).
//
// They used to be Redis keys written AFTER the usage was committed here. A
// crash between the two, a failed SET or a flushed Redis left usage without its
// marker, and the next tick billed the same bytes again. In this table a marker
// is written in the same transaction as the usage it stands for (AddTrafficTick),
// so the two cannot come apart.
//
// key is the old Redis key verbatim, so there is one naming scheme and a row
// can be seeded from the key it replaces.
func applyTrafficSeen(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS traffic_seen (
		key        TEXT PRIMARY KEY,
		val        BIGINT NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("traffic_seen: %w", err)
	}
	return nil
}
