package database

import (
	"database/sql"
	"fmt"
)

// applySolderTenancySchema gives every account its own Solder namespace.
//
// One Solder instance used to serve every tenant from one URL, which upstream
// Solder can assume because an install belongs to one operator and this one does
// not. Two things followed from that and both are fixed here:
//
//   - packs.solder_slug was globally UNIQUE, so the pack names were first come,
//     first served ACROSS CUSTOMERS. The second tenant to want "skyfactory"
//     could not have it. Uniqueness moves to (owner_id, solder_slug), which is
//     only sound because the read path is now addressed per account too - a bare
//     slug on a shared URL would otherwise match several packs.
//
//   - the public pack list was global, so every tenant's public packs were
//     listed together for anyone who opened the URL.
//
// users.solder_handle is what addresses an account: /solder/u/{handle}/api/.
// Deliberately NOT the username, which is mutable here (UPDATE users SET
// username) - a rename would break every linked Technic modpack and every
// launcher that already installed one, since the Platform stores the Solder URL
// per modpack.
//
// Additive + idempotent. Existing rows are globally unique already, so the
// narrower index holds for them trivially.
func applySolderTenancySchema(db *sql.DB) error {
	for _, q := range []string{
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS solder_handle VARCHAR(64) NOT NULL DEFAULT ''`,
		// Partial, like the slug index below: every account that has not chosen
		// one holds '', and '' is not a handle anybody can address.
		`CREATE UNIQUE INDEX IF NOT EXISTS users_solder_handle_uniq ON users (solder_handle) WHERE solder_handle <> ''`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("solder tenancy: users: %w", err)
		}
	}

	// Order matters: create the per-owner index BEFORE dropping the global one,
	// so there is no window in which two tenants could both take a slug. A
	// failure between the two leaves the stricter rule in place, which is the
	// safe direction.
	for _, q := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS packs_owner_solder_slug_uniq ON packs (owner_id, solder_slug) WHERE solder_slug <> ''`,
		`DROP INDEX IF EXISTS packs_solder_slug_uniq`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("solder tenancy: packs: %w", err)
		}
	}
	return nil
}
