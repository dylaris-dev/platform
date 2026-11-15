package database

import (
	"database/sql"
	"fmt"
)

// applyVoicePublicHost records the white-label voice name: a name on the
// customer's own domain that Core verified points at the voice port's region,
// written into the voice mod instead of our regional name once it is
// 'active'. public_state is empty (no name, our name is used), 'pending' (not
// verified yet), 'active' or 'fallen_back' (it stopped pointing at us and the
// mod was switched back to our name). public_state_at is when public_state
// last changed (NULL with no name): the panel's "switched back on <date>".
// remote_routes.voice_public_host is the
// mod host the provider named when it is not voice_host. The relay path never
// reads any of them: the node and the link keep resolving OUR name.
//
// Locks: each ADD COLUMN with a constant default is catalog-only (no rewrite),
// a brief ACCESS EXCLUSIVE on three small tables (one row per voice address).
// The CHECK is added once, guarded by name; every existing row holds the empty state and
// passes it. An older Core replica never reads or writes these columns: its
// INSERT leaves the defaults and its same-region UPDATE does not touch them.
func applyVoicePublicHost(db *sql.DB) error {
	for _, q := range []string{
		`ALTER TABLE voice_ports ADD COLUMN IF NOT EXISTS public_host TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE voice_ports ADD COLUMN IF NOT EXISTS public_state TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE link_route_voice ADD COLUMN IF NOT EXISTS public_host TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE link_route_voice ADD COLUMN IF NOT EXISTS public_state TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE voice_ports ADD COLUMN IF NOT EXISTS public_state_at TIMESTAMPTZ`,
		`ALTER TABLE link_route_voice ADD COLUMN IF NOT EXISTS public_state_at TIMESTAMPTZ`,
		`ALTER TABLE remote_routes ADD COLUMN IF NOT EXISTS voice_public_host TEXT NOT NULL DEFAULT ''`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'voice_ports_public_state_check' AND conrelid = 'voice_ports'::regclass) THEN
				ALTER TABLE voice_ports ADD CONSTRAINT voice_ports_public_state_check CHECK (public_state IN ('', 'pending', 'active', 'fallen_back'));
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'link_route_voice_public_state_check' AND conrelid = 'link_route_voice'::regclass) THEN
				ALTER TABLE link_route_voice ADD CONSTRAINT link_route_voice_public_state_check CHECK (public_state IN ('', 'pending', 'active', 'fallen_back'));
			END IF;
		END $$`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("voice public host: %w", err)
		}
	}
	return nil
}
