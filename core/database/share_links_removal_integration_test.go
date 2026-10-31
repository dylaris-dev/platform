package database

import "testing"

// Build share links are removed. One boot revokes every link and drops the
// sub-feature's setting; the rows stay, and the next boot finds nothing to do.
func TestShareLinksRemovalRevokesEveryLinkIntegration(t *testing.T) {
	db := freshSchemaDB(t)

	var userID string
	if err := db.QueryRow(`INSERT INTO users (username, password, role) VALUES ('share-owner', 'x', 'user') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var packID, buildID int
	if err := db.QueryRow(`INSERT INTO packs (owner_id, internal_name, internal_slug) VALUES ($1, 'P', 'p') RETURNING id`, userID).Scan(&packID); err != nil {
		t.Fatalf("seed pack: %v", err)
	}
	if err := db.QueryRow(`INSERT INTO pack_builds (pack_id, version_string) VALUES ($1, '1.0') RETURNING id`, packID).Scan(&buildID); err != nil {
		t.Fatalf("seed build: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO share_links (build_id, kind, token, created_by) VALUES ($1, 'client-mrpack', 'live-token', $2)`,
		`INSERT INTO share_links (build_id, kind, token, created_by, revoked) VALUES ($1, 'server-pack', 'old-token', $2, TRUE)`,
	} {
		if _, err := db.Exec(q, buildID, userID); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ('modpack_share_links_enabled', 'true')`); err != nil {
		t.Fatalf("seed setting: %v", err)
	}

	for round := 1; round <= 2; round++ {
		if err := applyShareLinksRemoval(db); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}

	var total, live int
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE NOT revoked) FROM share_links`).Scan(&total, &live); err != nil {
		t.Fatalf("read links: %v", err)
	}
	if total != 2 || live != 0 {
		t.Errorf("share_links: %d rows, %d not revoked; want 2 rows, all revoked", total, live)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'modpack_share_links_enabled'`).Scan(&n); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if n != 0 {
		t.Errorf("modpack_share_links_enabled is still set")
	}
}
