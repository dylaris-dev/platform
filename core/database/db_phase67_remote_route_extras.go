package database

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// applyRemoteRouteExtras records voice and Bedrock per remote_routes row: the
// provider's voice port (NULL = voice off), the voice mod whose config the node
// wrote, and the edition in services.EditionValue's words (empty = Java only).
// The two partial UNIQUE indexes are what keeps a server at one voice address
// and one Bedrock address even when two requests race.
//
// Locks: each ADD COLUMN is catalog-only (NULL or a constant default, no
// rewrite), a brief ACCESS EXCLUSIVE on remote_routes; existing rows read
// "voice off, Java only", which is what they had. The plain CREATE UNIQUE
// INDEX holds a SHARE lock on remote_routes (reads go on, writes wait) for the
// build; the table holds one row per remote address, a few hundred at most, and
// is empty on a Core that is not in remote mode.
//
// The edition is held to empty, 'both' and 'bedrock' by a CHECK, and the Bedrock
// index names those two values instead of "anything non-empty", so a value
// written by some later code can neither slip past the one-address rule nor
// count as Bedrock by accident. An index left with the first, looser predicate
// (a dev database that booted the unpublished first cut) is rebuilt.
func applyRemoteRouteExtras(db *sql.DB) error {
	for _, q := range []string{
		`ALTER TABLE remote_routes ADD COLUMN IF NOT EXISTS voice_port INTEGER`,
		`ALTER TABLE remote_routes ADD COLUMN IF NOT EXISTS voice_mod TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE remote_routes ADD COLUMN IF NOT EXISTS edition TEXT NOT NULL DEFAULT ''`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'remote_routes_edition_check' AND conrelid = 'remote_routes'::regclass) THEN
				ALTER TABLE remote_routes ADD CONSTRAINT remote_routes_edition_check CHECK (edition IN ('', 'both', 'bedrock'));
			END IF;
		END $$`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_routes_one_voice ON remote_routes (server_uuid) WHERE voice_port IS NOT NULL`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("remote route extras: %w", err)
		}
	}
	var def string
	err := db.QueryRow(`SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_remote_routes_one_bedrock'`).Scan(&def)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("remote route extras: read the Bedrock index: %w", err)
	case !strings.Contains(def, "'bedrock'"):
		if _, err := db.Exec(`DROP INDEX idx_remote_routes_one_bedrock`); err != nil {
			return fmt.Errorf("remote route extras: drop the old Bedrock index: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_routes_one_bedrock ON remote_routes (server_uuid) WHERE edition IN ('both', 'bedrock')`); err != nil {
		return fmt.Errorf("remote route extras: %w", err)
	}
	return nil
}
