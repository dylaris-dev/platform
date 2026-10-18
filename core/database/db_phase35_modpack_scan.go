package database

import (
	"database/sql"
	"fmt"
)

// applyModpackScanSchema lets the Content tab tell a Technic or uploaded pack
// apart from a plain loader server. Neither installer leaves a Modrinth
// manifest behind, so their members can only be found by hashing the jars on
// disk, which Core does lazily the first time the Content tab asks.
//
// modpack_scan is empty (nothing to scan), 'pending' (scan owed) or 'done'.
// modpack_jars is how many unmanaged jars that scan found in mods/, including
// the ones Modrinth does not know: a pack built from private jars is still a
// pack.
//
// modpack_scan_gen counts every time a scan is owed. A scan remembers the
// generation it started under and only records its result if no reinstall
// owed a new one meanwhile: 'pending' before and after a reinstall look the
// same, so the state alone could not tell a stale scan from a current one.
//
// The backfill marks every existing Technic and upload sub-server as owed a
// scan. An upload that installed a launcher over itself recorded the launcher
// ("forge") as its installer, so those are not reachable here; their next
// reinstall or setup schedules the scan instead.
//
// Additive and idempotent: the backfill only touches rows still empty.
func applyModpackScanSchema(db *sql.DB) error {
	for _, q := range []string{
		`ALTER TABLE sub_server_installs ADD COLUMN IF NOT EXISTS modpack_scan TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sub_server_installs ADD COLUMN IF NOT EXISTS modpack_jars INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sub_server_installs ADD COLUMN IF NOT EXISTS modpack_scan_gen INTEGER NOT NULL DEFAULT 0`,
		`UPDATE sub_server_installs SET modpack_scan='pending'
			WHERE modpack_scan='' AND installer_type IN ('technic','upload','upload-zip')`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("modpack scan: %s: %w", q, err)
		}
	}
	return nil
}
