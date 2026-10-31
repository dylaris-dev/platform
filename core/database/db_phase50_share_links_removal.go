package database

import (
	"database/sql"
	"fmt"
)

// applyShareLinksRemoval retires the removed build share links: every link is
// marked revoked and the sub-feature's setting is dropped. The table stays for
// now, so a rollback finds its rows and the revocation stays on record. Both
// statements are no-ops once they have run, so it is safe on every boot.
func applyShareLinksRemoval(db *sql.DB) error {
	for _, q := range []string{
		`UPDATE share_links SET revoked = TRUE WHERE revoked = FALSE`,
		`DELETE FROM settings WHERE key = 'modpack_share_links_enabled'`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("share links removal: %w", err)
		}
	}
	return nil
}
