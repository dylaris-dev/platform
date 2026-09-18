package database

import (
	"database/sql"
	"fmt"
)

// applySolderRemoval takes out what the Technic Solder integration left in the
// schema. Solder was removed as a feature on 2026-09-18. Measured on
// production before the change: no pack, no build, no Solder client; one
// Technic key and one account handle, both going with it.
//
// It runs on every boot like the rest of ensureSchema, and every statement is
// IF EXISTS, so after the first boot it does nothing. The matching CREATE and
// ADD COLUMN statements were removed in the same change - otherwise the next
// boot would recreate exactly what this drops.
//
// Order is dependency order and deliberately WITHOUT CASCADE: pack_clients
// refers to solder_clients, and nothing else refers to any of these. A CASCADE
// would quietly take out an object nobody knew depended on them; without it,
// such an object stops the boot and gets looked at instead.
//
// THIS IS A ONE-WAY MIGRATION, by the owner's decision on 2026-09-18.
//
// A Core older than this change does NOT start against a database this step has
// run on. Its ensureSchema creates an index on packs.solder_slug at every boot,
// that fails once the column is gone, and main() exits on a schema error - so a
// rollback puts every Core replica into a restart loop. The older CREATE TABLE
// statements are IF NOT EXISTS and would re-create the dropped TABLES, but
// nothing in the older code ever re-adds the dropped COLUMNS.
//
// To roll back anyway, restore the columns first (empty; the data is gone by
// design), then start the older image:
//
//	ALTER TABLE packs ADD COLUMN IF NOT EXISTS solder_display_name VARCHAR(128) NOT NULL DEFAULT '';
//	ALTER TABLE packs ADD COLUMN IF NOT EXISTS solder_slug VARCHAR(128) NOT NULL DEFAULT '';
//	ALTER TABLE packs ADD COLUMN IF NOT EXISTS hidden BOOLEAN NOT NULL DEFAULT FALSE;
//	ALTER TABLE packs ADD COLUMN IF NOT EXISTS private BOOLEAN NOT NULL DEFAULT FALSE;
//	ALTER TABLE packs ADD COLUMN IF NOT EXISTS recommended_build VARCHAR(64) NOT NULL DEFAULT '';
//	ALTER TABLE packs ADD COLUMN IF NOT EXISTS latest_build VARCHAR(64) NOT NULL DEFAULT '';
//	ALTER TABLE packs ADD COLUMN IF NOT EXISTS icon_url TEXT NOT NULL DEFAULT '';
//	ALTER TABLE packs ADD COLUMN IF NOT EXISTS logo_url TEXT NOT NULL DEFAULT '';
//	ALTER TABLE packs ADD COLUMN IF NOT EXISTS background_url TEXT NOT NULL DEFAULT '';
//	ALTER TABLE packs ADD COLUMN IF NOT EXISTS icon_md5 VARCHAR(32) NOT NULL DEFAULT '';
//	ALTER TABLE packs ADD COLUMN IF NOT EXISTS logo_md5 VARCHAR(32) NOT NULL DEFAULT '';
//	ALTER TABLE packs ADD COLUMN IF NOT EXISTS background_md5 VARCHAR(32) NOT NULL DEFAULT '';
//	ALTER TABLE pack_builds ADD COLUMN IF NOT EXISTS solder_published BOOLEAN NOT NULL DEFAULT FALSE;
//	ALTER TABLE pack_builds ADD COLUMN IF NOT EXISTS solder_private BOOLEAN NOT NULL DEFAULT FALSE;
//	ALTER TABLE modversions ADD COLUMN IF NOT EXISTS url_override TEXT NOT NULL DEFAULT '';
//
// The same holds, briefly, for the rolling deploy itself: while an older replica
// is still up, its pack queries name columns this has dropped and fail. That
// reaches only the pack builder, which had no data on production.
func applySolderRemoval(db *sql.DB) error {
	stmts := []string{
		// The Technic launcher surface: client identities, the pack whitelist,
		// the keys technicpack.net verified against, and the launcher loader zips.
		`DROP TABLE IF EXISTS pack_clients`,
		`DROP TABLE IF EXISTS solder_clients`,
		`DROP TABLE IF EXISTS solder_keys`,
		`DROP TABLE IF EXISTS loaders`,

		// The per-account address under /solder/u/{handle}/.
		`DROP INDEX IF EXISTS users_solder_handle_uniq`,
		`ALTER TABLE users DROP COLUMN IF EXISTS solder_handle`,

		// Solder's own identity and launcher metadata on a pack. The pack
		// builder keeps its internal name and slug and its Modrinth identity.
		`DROP INDEX IF EXISTS packs_owner_solder_slug_uniq`,
		`DROP INDEX IF EXISTS packs_solder_slug_uniq`,
		`ALTER TABLE packs DROP COLUMN IF EXISTS solder_display_name`,
		`ALTER TABLE packs DROP COLUMN IF EXISTS solder_slug`,
		`ALTER TABLE packs DROP COLUMN IF EXISTS hidden`,
		`ALTER TABLE packs DROP COLUMN IF EXISTS private`,
		`ALTER TABLE packs DROP COLUMN IF EXISTS recommended_build`,
		`ALTER TABLE packs DROP COLUMN IF EXISTS latest_build`,
		`ALTER TABLE packs DROP COLUMN IF EXISTS icon_url`,
		`ALTER TABLE packs DROP COLUMN IF EXISTS logo_url`,
		`ALTER TABLE packs DROP COLUMN IF EXISTS background_url`,
		`ALTER TABLE packs DROP COLUMN IF EXISTS icon_md5`,
		`ALTER TABLE packs DROP COLUMN IF EXISTS logo_md5`,
		`ALTER TABLE packs DROP COLUMN IF EXISTS background_md5`,
		`ALTER TABLE pack_builds DROP COLUMN IF EXISTS solder_published`,
		`ALTER TABLE pack_builds DROP COLUMN IF EXISTS solder_private`,

		// A per-file download override only the Solder render ever read, and
		// every writer set to "".
		`ALTER TABLE modversions DROP COLUMN IF EXISTS url_override`,

		// The delivery-mode switch and the public mirror base. core_public_url
		// stays: a node installing a panel-built pack downloads it from there.
		`DELETE FROM settings WHERE key IN ('solder_delivery_mode', 'solder_mirror_url')`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("solder removal: %s: %w", q, err)
		}
	}
	return nil
}
