package database

import (
	"database/sql"
	"fmt"
)

// applyVoiceHost records the name a voice mod is told to connect to when it is
// not the address itself. link_route_voice.host is OUR regional voice name for
// a route-only address on the customer's own domain; remote_routes.voice_host
// is the name the provider answered for a remote address. Empty means the
// address's own domain, which is what every existing row has always used.
//
// Locks: each ADD COLUMN with a constant default is catalog-only (no rewrite),
// a brief ACCESS EXCLUSIVE on two small tables (one row per voice address).
// An older Core replica never reads or writes either column: its INSERT leaves
// the default and its same-region UPDATE does not touch it.
func applyVoiceHost(db *sql.DB) error {
	for _, q := range []string{
		`ALTER TABLE link_route_voice ADD COLUMN IF NOT EXISTS host TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE remote_routes ADD COLUMN IF NOT EXISTS voice_host TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("voice host: %w", err)
		}
	}
	return nil
}
