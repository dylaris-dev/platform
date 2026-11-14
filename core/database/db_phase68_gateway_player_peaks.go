package database

import (
	"database/sql"
	"fmt"
)

// applyGatewayPlayerPeaks creates the daily peak of concurrent players per
// gateway account (provider side). owner_id is core_link_routes.owner_id, TEXT
// there too, so there is no foreign key: a deleted account's rows age out with
// the retention purge. The three numbers are separate maxima of the UTC day;
// total is the highest Java+Bedrock seen in ONE sample, not java + bedrock.
//
// Locks: a new table and an index on it, nothing existing is touched.
func applyGatewayPlayerPeaks(db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS gateway_player_peaks (
			owner_id   TEXT        NOT NULL,
			day        DATE        NOT NULL,
			java       INTEGER     NOT NULL DEFAULT 0,
			bedrock    INTEGER     NOT NULL DEFAULT 0,
			total      INTEGER     NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (owner_id, day)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_player_peaks_day ON gateway_player_peaks (day)`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("gateway player peaks: %w", err)
		}
	}
	return nil
}
